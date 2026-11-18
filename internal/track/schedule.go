package track

import (
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
)

// serviceDay returns the moment that GTFS times on a service date count
// from: noon minus twelve hours, which is midnight except on days when the
// clocks change.
func serviceDay(date string, loc *time.Location) (time.Time, bool) {
	d, err := time.ParseInLocation("20060102", date, loc)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc).Add(-12 * time.Hour), true
}

// nearestServiceDay picks, for a trip update that gives no start date, the
// service day on which the trip's schedule falls closest to now.
func nearestServiceDay(first, last int, now time.Time, loc *time.Location) time.Time {
	var best time.Time
	bestGap := time.Duration(-1)
	local := now.In(loc)
	for offset := -1; offset <= 1; offset++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+offset, 12, 0, 0, 0, loc).Add(-12 * time.Hour)
		start, end := day.Add(time.Duration(first)*time.Second), day.Add(time.Duration(last)*time.Second)
		gap := time.Duration(0)
		switch {
		case now.Before(start):
			gap = start.Sub(now)
		case now.After(end):
			gap = now.Sub(end)
		}
		if bestGap < 0 || gap < bestGap {
			best, bestGap = day, gap
		}
	}
	return best
}

// needsSchedule reports whether a trip update cannot be read on its own:
// it gives a stop only a delay, or only a stop sequence, or gives one delay
// for the whole trip.
func needsSchedule(tu *gtfs.TripUpdate) bool {
	for _, u := range tu.GetStopTimeUpdate() {
		if u.GetScheduleRelationship() != gtfs.TripUpdate_StopTimeUpdate_SCHEDULED {
			continue
		}
		if u.StopId == nil || (u.GetArrival().GetTime() == 0 && u.GetDeparture().GetTime() == 0) {
			return true
		}
	}
	return len(tu.GetStopTimeUpdate()) == 0 && tu.Delay != nil
}

// scheduledTimes returns each call's arrival and departure in seconds from
// the start of the service day. Calls the schedule gives no time for are
// spaced evenly between their timed neighbours.
func scheduledTimes(schedule []static.StopTime) (arrivals, departures []int, ok bool) {
	arrivals, departures = make([]int, len(schedule)), make([]int, len(schedule))
	known := -1
	for i, st := range schedule {
		arrivals[i], departures[i] = st.Arrival, st.Departure
		if arrivals[i] < 0 {
			arrivals[i] = departures[i]
		}
		if departures[i] < 0 {
			departures[i] = arrivals[i]
		}
		if arrivals[i] < 0 {
			continue
		}
		for j := known + 1; j < i; j++ {
			at := arrivals[i]
			if known >= 0 {
				at = departures[known] + (arrivals[i]-departures[known])*(j-known)/(i-known)
			}
			arrivals[j], departures[j] = at, at
		}
		known = i
	}
	if known < 0 {
		return nil, nil, false
	}
	for j := known + 1; j < len(schedule); j++ {
		arrivals[j], departures[j] = departures[known], departures[known]
	}
	return arrivals, departures, true
}

// scheduledVisits works out a trip's visits from its schedule and the
// delays in its trip update, for an update that cannot be read on its own.
// A stop with no update of its own takes the delay of the update before it.
// Stops ahead of the first update take that update's delay.
//
// It reports false if the update can be read on its own, or does not line
// up with a trip in the static feed.
func scheduledVisits(tu *gtfs.TripUpdate, data *static.Data, now time.Time) ([]visit, bool) {
	if data.Location == nil || !needsSchedule(tu) {
		return nil, false
	}
	schedule := data.Schedule(tu.GetTrip().GetTripId())
	arrivals, departures, ok := scheduledTimes(schedule)
	if !ok {
		return nil, false
	}
	day, ok := serviceDay(tu.GetTrip().GetStartDate(), data.Location)
	if !ok {
		day = nearestServiceDay(departures[0], arrivals[len(schedule)-1], now, data.Location)
	}

	updates := make([]*gtfs.TripUpdate_StopTimeUpdate, len(schedule))
	next := 0
	for _, u := range tu.GetStopTimeUpdate() {
		i := next
		for i < len(schedule) && !(u.StopSequence != nil && schedule[i].Sequence == u.GetStopSequence()) && !(u.StopSequence == nil && schedule[i].Stop == u.GetStopId()) {
			i++
		}
		if i == len(schedule) {
			return nil, false
		}
		updates[i], next = u, i+1
	}

	at := func(seconds int, delay time.Duration) time.Time {
		return day.Add(time.Duration(seconds)*time.Second + delay)
	}
	event := func(e *gtfs.TripUpdate_StopTimeEvent, seconds int, delay *time.Duration) bool {
		switch {
		case e.GetTime() != 0:
			*delay = time.Unix(e.GetTime(), 0).Sub(day.Add(time.Duration(seconds) * time.Second))
		case e != nil && e.Delay != nil:
			*delay = time.Duration(e.GetDelay()) * time.Second
		default:
			return false
		}
		return true
	}

	delay := time.Duration(tu.GetDelay()) * time.Second
	first := -1
	var firstDelay time.Duration
	visits := make([]visit, 0, len(schedule))
	for i, st := range schedule {
		arrivalDelay := delay
		if u := updates[i]; u != nil && u.GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SCHEDULED {
			arrives := event(u.Arrival, arrivals[i], &delay)
			arrivalDelay = delay
			departs := event(u.Departure, departures[i], &delay)
			if !arrives && departs && first < 0 {
				arrivalDelay = delay
			}
			if (arrives || departs) && first < 0 {
				first, firstDelay = len(visits), arrivalDelay
			}
		}
		if updates[i].GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SKIPPED || !data.HasStop(st.Stop) {
			continue
		}
		sequence := st.Sequence
		visits = append(visits, visit{st.Stop, &sequence, at(arrivals[i], arrivalDelay), at(departures[i], delay)})
	}
	if tu.Delay != nil {
		return visits, true
	}
	if first < 0 {
		return nil, false
	}
	for i := 0; i < first; i++ {
		visits[i].arrival = visits[i].arrival.Add(firstDelay)
		visits[i].departure = visits[i].departure.Add(firstDelay)
	}
	return visits, true
}
