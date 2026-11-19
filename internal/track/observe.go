package track

import (
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

// An observation is where one feed snapshot says a train is: either standing
// at a stop, or on its way to one.
type observation struct {
	at string

	// from is set only when the feed or the schedule names the stop the
	// train left, and departed only when the feed says when it left.
	from     string
	departed time.Time
	to       string
	eta      time.Time
	incoming bool
	sequence *uint32

	// finished is a train at its last stop, which feeds can list for hours.
	finished bool
}

// observe reads a train's whereabouts from its vehicle entity and trip
// update. The feeds disagree on what current_status means, so the trip
// update decides:
//   - the numbered lines and the L report STOPPED_AT only at the first stop
//     still listed in the trip update;
//   - the lettered lines keep reporting STOPPED_AT the previous stop while
//     the train is moving, and the stop drops out of the trip update once
//     the train has left it.
//
// Stops missing from the static feed are skipped over.
func observe(v *gtfs.VehiclePosition, tu *gtfs.TripUpdate, known func(stop string) bool) (observation, bool) {
	updates := tu.GetStopTimeUpdate()
	stop := v.GetStopId()
	stopped := v.GetCurrentStatus() == gtfs.VehiclePosition_STOPPED_AT
	listedFirst := len(updates) == 0 || updates[0].GetStopId() == stop

	if stopped && listedFirst && known(stop) {
		return observation{at: stop, sequence: v.CurrentStopSequence, finished: tu != nil && len(updates) <= 1}, true
	}

	var o observation
	if stopped && !listedFirst && known(stop) {
		o.from = stop
	}
	for _, u := range updates {
		if !known(u.GetStopId()) {
			continue
		}
		o.to = u.GetStopId()
		o.sequence = u.StopSequence
		if u.GetStopId() == stop && o.sequence == nil {
			o.sequence = v.CurrentStopSequence
		}
		if at := u.GetArrival().GetTime(); at != 0 {
			o.eta = time.Unix(at, 0)
		} else if at := u.GetDeparture().GetTime(); at != 0 {
			o.eta = time.Unix(at, 0)
		}
		break
	}
	switch {
	case o.to != "" && o.to != o.from:
		o.incoming = v.GetCurrentStatus() == gtfs.VehiclePosition_INCOMING_AT && o.to == stop
		return o, true
	case o.from != "":
		return observation{at: o.from, sequence: v.CurrentStopSequence}, true
	case known(stop):
		return observation{to: stop, sequence: v.CurrentStopSequence}, true
	}
	return observation{}, false
}

// A train is shown standing at its last stop for this long after arriving.
const terminalDwell = 3 * time.Minute

// A visit is one stop of a trip, with the times the train is predicted or
// known to be there.
type visit struct {
	stop               string
	sequence           *uint32
	arrival, departure time.Time
}

// listedVisits takes a trip's visits from the absolute times in its trip
// update. Skipped stops, stops with no time and stops missing from the
// static feed are left out.
func listedVisits(tu *gtfs.TripUpdate, known func(stop string) bool) []visit {
	var visits []visit
	for _, u := range tu.GetStopTimeUpdate() {
		if u.GetScheduleRelationship() != gtfs.TripUpdate_StopTimeUpdate_SCHEDULED || !known(u.GetStopId()) {
			continue
		}
		arrival, departure := u.GetArrival().GetTime(), u.GetDeparture().GetTime()
		if arrival == 0 {
			arrival = departure
		}
		if departure == 0 {
			departure = arrival
		}
		if arrival == 0 {
			continue
		}
		visits = append(visits, visit{u.GetStopId(), u.StopSequence, time.Unix(arrival, 0), time.Unix(departure, 0)})
	}
	return visits
}

// observeVisits reads a train's whereabouts from the times of its visits
// alone, for feeds with no vehicle entities. The train is between the last
// stop whose time has passed and the first whose time has not.
//
// An observation with neither at nor from is a train whose first listed
// stop is still ahead: feeds that drop stops as they are passed look like
// this, and so does a trip that has not started.
func observeVisits(visits []visit, now time.Time) (observation, bool) {
	if len(visits) == 0 {
		return observation{}, false
	}
	next := 0
	for next < len(visits) && !visits[next].arrival.After(now) {
		next++
	}
	if next == len(visits) {
		last := visits[next-1]
		if now.Sub(last.arrival) > terminalDwell {
			return observation{}, false
		}
		return observation{at: last.stop, sequence: last.sequence}, true
	}
	o := observation{to: visits[next].stop, eta: visits[next].arrival, sequence: visits[next].sequence}
	if next == 0 {
		return o, true
	}
	prev := visits[next-1]
	if prev.departure.After(now) {
		return observation{at: prev.stop, sequence: prev.sequence}, true
	}
	o.from, o.departed = prev.stop, prev.departure
	return o, true
}
