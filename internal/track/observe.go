package track

import (
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

// An observation is where one snapshot puts a train: standing at a stop, or
// on its way to one.
type observation struct {
	at string

	// from is set only when the feed or schedule names the stop the train
	// left, and departed only when the feed says when.
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
// update. Feeds disagree on what current_status means, so the trip update
// decides:
//   - some report STOPPED_AT only at the first stop still listed;
//   - others keep reporting STOPPED_AT the previous stop while the train
//     moves, dropping that stop from the trip update once it has left.
//
// Stops listed before the one the vehicle entity names have been passed.
//
// Stops missing from the static feed are skipped.
func observe(v *gtfs.VehiclePosition, tu *gtfs.TripUpdate, known func(stop string) bool) (observation, bool) {
	updates := tu.GetStopTimeUpdate()
	stop := v.GetStopId()
	for i, u := range updates {
		if u.GetStopId() == stop {
			updates = updates[i:]
			break
		}
	}
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
		o.eta = due(u)
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

func due(u *gtfs.TripUpdate_StopTimeUpdate) time.Time {
	if at := u.GetArrival().GetTime(); at != 0 {
		return time.Unix(at, 0)
	}
	if at := u.GetDeparture().GetTime(); at != 0 {
		return time.Unix(at, 0)
	}
	return time.Time{}
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

// listedVisits takes visits from the absolute times in a trip update,
// leaving out stops that are skipped, untimed or missing from the static
// feed.
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

// observeVisits places a train by its visit times alone, for feeds with no
// vehicle entities: between the last stop whose time has passed and the
// first whose has not.
//
// With neither at nor from, the first listed stop is still ahead: the feed
// drops stops as they are passed, or the trip has not started.
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
