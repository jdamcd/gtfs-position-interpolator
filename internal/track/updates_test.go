package track

import (
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

func TestObserveVisits(t *testing.T) {
	known := func(stop string) bool { return stop != "999N" }
	at := func(seconds int) time.Time { return t0.Add(time.Duration(seconds) * time.Second) }
	dwell := func(stop string, arrive, depart time.Time) *gtfs.TripUpdate_StopTimeUpdate {
		return &gtfs.TripUpdate_StopTimeUpdate{
			StopId:    proto.String(stop),
			Arrival:   &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(arrive.Unix())},
			Departure: &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(depart.Unix())},
		}
	}
	skipped := dwell("102N", at(-20), at(-20))
	skipped.ScheduleRelationship = gtfs.TripUpdate_StopTimeUpdate_SKIPPED.Enum()

	for name, c := range map[string]struct {
		updates []*gtfs.TripUpdate_StopTimeUpdate
		want    observation
		none    bool
	}{
		"between two listed stops": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("101N", at(-90), at(-60)), dwell("102N", at(40), at(70))},
			want:    observation{from: "101N", departed: at(-60), to: "102N", eta: at(40)},
		},
		"standing between arrival and departure": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("101N", at(-10), at(20)), dwell("102N", at(140), at(140))},
			want:    observation{at: "101N"},
		},
		"first listed stop still ahead": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("102N", at(40), at(40)), dwell("103N", at(160), at(160))},
			want:    observation{to: "102N", eta: at(40)},
		},
		"just arrived at the last stop": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("103N", at(-200), at(-200)), dwell("104N", at(-30), at(-30))},
			want:    observation{at: "104N"},
		},
		"long finished": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("104N", at(-300), at(-300))},
			none:    true,
		},
		"skipped and unknown stops are passed through": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{dwell("101N", at(-90), at(-60)), skipped, dwell("999N", at(10), at(10)), dwell("103N", at(100), at(100))},
			want:    observation{from: "101N", departed: at(-60), to: "103N", eta: at(100)},
		},
		"no usable stops": {
			updates: []*gtfs.TripUpdate_StopTimeUpdate{{StopId: proto.String("101N")}},
			none:    true,
		},
	} {
		got, ok := observeVisits(listedVisits(&gtfs.TripUpdate{StopTimeUpdate: c.updates}, known), t0)
		if ok == c.none {
			t.Errorf("%s: observed %v, want %v", name, ok, !c.none)
			continue
		}
		if !c.none && got != c.want {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
}

func TestTripUpdatesAloneDrawATrain(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("feed", message(t0,
		tripUpdate("1", "L1", call{"102N", t0.Add(60 * time.Second)}, call{"103N", t0.Add(180 * time.Second)}),
	), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "101N", 500, inTransit, "102N")
	checkPlaced(t, only(t, tracker.Feed(t0.Add(30*time.Second))), data, "101N", 750, inTransit, "102N")

	tracker.Apply("feed", message(t0.Add(60*time.Second),
		tripUpdate("1", "L1", call{"103N", t0.Add(180 * time.Second)}),
	), t0.Add(60*time.Second))
	checkPlaced(t, only(t, tracker.Feed(t0.Add(120*time.Second))), data, "102N", 500, inTransit, "103N")
}

func TestListedPastStopTimesTheDeparture(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("feed", message(t0,
		tripUpdate("1", "L1", call{"101N", t0.Add(-30 * time.Second)}, call{"102N", t0.Add(30 * time.Second)}),
	), t0)
	checkPlaced(t, only(t, tracker.Feed(t0)), data, "101N", 500, inTransit, "102N")
}

func TestTripsThatHaveNotStartedAreLeftOut(t *testing.T) {
	data := network(t)
	for name, entity := range map[string]*gtfs.FeedEntity{
		"waiting at its first stop":                    tripUpdate("1", "L1", call{"101N", t0.Add(60 * time.Second)}, call{"102N", t0.Add(180 * time.Second)}),
		"not yet due to leave the stop before":         tripUpdate("1", "L1", call{"102N", t0.Add(300 * time.Second)}),
		"unscheduled trip at a stop nothing runs into": tripUpdate("1", "EXTRA", call{"101N", t0.Add(20 * time.Second)}),
	} {
		tracker := New(data, config)
		tracker.Apply("feed", message(t0, entity), t0)
		if got := len(tracker.Feed(t0).Entity); got != 0 {
			t.Errorf("%s: %d vehicles, want none", name, got)
		}
	}
}

func TestTripAppearsOnceItIsDueToHaveLeft(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	update := tripUpdate("1", "L1", call{"102N", t0.Add(300 * time.Second)})
	tracker.Apply("feed", message(t0, update), t0)
	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Fatalf("%d vehicles before the trip is due to leave, want none", got)
	}
	later := t0.Add(240 * time.Second)
	tracker.Apply("feed", message(later, update), later)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "101N", 500, inTransit, "102N")
}

func TestUnscheduledTripIsDrawnFromTheUsualPreviousStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("feed", message(t0, tripUpdate("1", "EXTRA", call{"103N", t0.Add(60 * time.Second)})), t0)
	checkPlaced(t, only(t, tracker.Feed(t0)), data, "102N", 500, inTransit, "103N")
}

func TestCancelledTripsAreLeftOut(t *testing.T) {
	tracker := New(network(t), config)
	update := tripUpdate("1", "L1", call{"102N", t0.Add(60 * time.Second)})
	update.TripUpdate.Trip.ScheduleRelationship = gtfs.TripDescriptor_CANCELED.Enum()
	tracker.Apply("feed", message(t0, update), t0)
	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Errorf("%d vehicles, want none", got)
	}
}

func TestTripUpdateFillsInRouteAndVehicle(t *testing.T) {
	tracker := New(network(t), config)
	update := tripUpdate("", "X1", call{"103N", t0.Add(100 * time.Second)})
	update.TripUpdate.Trip.RouteId = nil
	update.TripUpdate.Vehicle = &gtfs.VehicleDescriptor{Id: proto.String("car-7")}
	tracker.Apply("feed", message(t0, update), t0)

	v := only(t, tracker.Feed(t0))
	if got := v.GetTrip().GetRouteId(); got != "2" {
		t.Errorf("route %q, want 2 from the schedule", got)
	}
	if got := v.GetVehicle().GetId(); got != "car-7" {
		t.Errorf("vehicle %q, want car-7", got)
	}
	if update.TripUpdate.Trip.RouteId != nil {
		t.Error("the upstream trip descriptor was modified")
	}
}

func TestFeedWithVehiclesIgnoresTripsWithoutOne(t *testing.T) {
	tracker := New(network(t), config)
	unassigned := tripUpdate("1", "L2", call{"102N", t0.Add(60 * time.Second)})
	tracker.Apply("feed", message(t0, vehicle("1", "L1", "103N", stopped, t0), unassigned), t0)
	if v := only(t, tracker.Feed(t0)); v.GetTrip().GetTripId() != "L1" {
		t.Errorf("drew trip %s, want only L1", v.GetTrip().GetTripId())
	}

	later := t0.Add(15 * time.Second)
	tracker.Apply("feed", message(later, unassigned), later)
	if got := len(tracker.Feed(later).Entity); got != 0 {
		t.Errorf("%d vehicles from a snapshot with no vehicle entities, want none", got)
	}
}

func TestVehicleEntityFillsInRouteAndVehicle(t *testing.T) {
	tracker := New(network(t), config)
	entity := vehicle("", "X1", "103N", stopped, t0)
	entity.Vehicle.Trip.RouteId = nil
	update := tripUpdate("", "X1", call{"103N", t0})
	update.TripUpdate.Vehicle = &gtfs.VehicleDescriptor{Id: proto.String("car-7")}
	tracker.Apply("feed", message(t0, entity, update), t0)

	v := only(t, tracker.Feed(t0))
	if got := v.GetTrip().GetRouteId(); got != "2" {
		t.Errorf("route %q, want 2 from the schedule", got)
	}
	if got := v.GetVehicle().GetId(); got != "car-7" {
		t.Errorf("vehicle %q, want car-7 from the trip update", got)
	}
	if entity.Vehicle.Trip.RouteId != nil {
		t.Error("the upstream trip descriptor was modified")
	}
}

func TestTripUpdatesWithoutATripIDAreLeftOut(t *testing.T) {
	tracker := New(network(t), config)
	var entities []*gtfs.FeedEntity
	for range 2 {
		update := tripUpdate("1", "", call{"103N", t0.Add(60 * time.Second)})
		update.TripUpdate.Trip.TripId = nil
		entities = append(entities, update)
	}
	tracker.Apply("feed", message(t0, entities...), t0)
	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Errorf("%d vehicles, want none", got)
	}
}

func TestFeedWithoutAHeaderTimestampIsTimedFromItsFetch(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	msg := message(t0, tripUpdate("1", "L1", call{"102N", t0.Add(60 * time.Second)}))
	msg.Header.Timestamp = nil
	tracker.Apply("feed", msg, t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "101N", 500, inTransit, "102N")
	if got := tracker.Status()["feed"].Header; !got.Equal(t0) {
		t.Errorf("feed header %v, want the fetch time %v", got, t0)
	}
}
