package track

import (
	"math"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/geo"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static/statictest"
)

const (
	stopped   = gtfs.VehiclePosition_STOPPED_AT
	inTransit = gtfs.VehiclePosition_IN_TRANSIT_TO
	incoming  = gtfs.VehiclePosition_INCOMING_AT
)

var t0 = time.Unix(1_794_862_851, 0)

var config = Config{MaxAge: 10 * time.Minute}

func network(t *testing.T) *static.Data {
	t.Helper()
	data, err := static.Parse(statictest.Zip(t, statictest.Network()))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func trip(route, id string) *gtfs.TripDescriptor {
	return &gtfs.TripDescriptor{
		TripId:    proto.String(id),
		RouteId:   proto.String(route),
		StartDate: proto.String("20261116"),
	}
}

func vehicle(route, id, stop string, status gtfs.VehiclePosition_VehicleStopStatus, at time.Time) *gtfs.FeedEntity {
	return &gtfs.FeedEntity{
		Id: proto.String("v" + id),
		Vehicle: &gtfs.VehiclePosition{
			Trip:          trip(route, id),
			StopId:        proto.String(stop),
			CurrentStatus: status.Enum(),
			Timestamp:     proto.Uint64(uint64(at.Unix())),
		},
	}
}

type call struct {
	stop string
	at   time.Time
}

func tripUpdate(route, id string, calls ...call) *gtfs.FeedEntity {
	tu := &gtfs.TripUpdate{Trip: trip(route, id)}
	for _, c := range calls {
		event := &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(c.at.Unix())}
		tu.StopTimeUpdate = append(tu.StopTimeUpdate, &gtfs.TripUpdate_StopTimeUpdate{
			StopId:    proto.String(c.stop),
			Arrival:   event,
			Departure: event,
		})
	}
	return &gtfs.FeedEntity{Id: proto.String("t" + id), TripUpdate: tu}
}

func message(header time.Time, entities ...*gtfs.FeedEntity) *gtfs.FeedMessage {
	return &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("1.0"), Timestamp: proto.Uint64(uint64(header.Unix()))},
		Entity: entities,
	}
}

func only(t *testing.T, feed *gtfs.FeedMessage) *gtfs.VehiclePosition {
	t.Helper()
	if len(feed.Entity) != 1 {
		t.Fatalf("got %d entities, want 1", len(feed.Entity))
	}
	return feed.Entity[0].Vehicle
}

func checkPlaced(t *testing.T, v *gtfs.VehiclePosition, data *static.Data, from string, metres float64, status gtfs.VehiclePosition_VehicleStopStatus, stop string) {
	t.Helper()
	p := geo.Point{Lat: float64(v.Position.GetLatitude()), Lon: float64(v.Position.GetLongitude())}
	if got := geo.Distance(data.Stops[from], p); math.Abs(got-metres) > 5 {
		t.Errorf("train is %.0f m from %s, want %.0f m", got, from, metres)
	}
	if v.GetCurrentStatus() != status || v.GetStopId() != stop {
		t.Errorf("status %v %s, want %v %s", v.GetCurrentStatus(), v.GetStopId(), status, stop)
	}
}

func TestMovingTrainIsPlacedBySchedule(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "103N", inTransit, t0),
		tripUpdate("1", "L1", call{"103N", t0.Add(60 * time.Second)}, call{"104N", t0.Add(180 * time.Second)}),
	), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "102N", 500, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(t0.Add(30*time.Second))), data, "102N", 750, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(t0.Add(5*time.Minute))), data, "103N", 0, inTransit, "103N")
}

func TestExpressTrainComesFromItsOwnPreviousStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0,
		vehicle("2", "X1", "103N", incoming, t0),
		tripUpdate("2", "X1", call{"103N", t0.Add(50 * time.Second)}),
	), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "101N", 1500, incoming, "103N")
}

func TestStoppedAtFirstListedStopIsAtTheStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0,
		vehicle("1", "L1", "102N", stopped, t0.Add(-10*time.Second)),
		tripUpdate("1", "L1", call{"102N", t0.Add(-10 * time.Second)}, call{"103N", t0.Add(110 * time.Second)}),
	), t0)

	v := only(t, tracker.Feed(t0))
	checkPlaced(t, v, data, "102N", 0, stopped, "102N")
	if got, want := v.GetTimestamp(), uint64(t0.Unix()-10); got != want {
		t.Errorf("vehicle timestamp %d, want the upstream value %d", got, want)
	}
}

func TestStoppedAtUnlistedStopHasLeftIt(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0,
		vehicle("2", "X1", "102N", stopped, t0.Add(-60*time.Second)),
		tripUpdate("2", "X1", call{"103N", t0.Add(30 * time.Second)}),
	), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "102N", 750, inTransit, "103N")
}

func TestUnknownStopsArePassedThrough(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("nqrw", message(t0,
		vehicle("1", "L1", "X99N", inTransit, t0),
		tripUpdate("1", "L1", call{"X99N", t0.Add(20 * time.Second)}, call{"103N", t0.Add(60 * time.Second)}),
	), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "102N", 500, inTransit, "103N")
}

func TestTrainsThatHaveNotStartedAreLeftOut(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("ace", message(t0,
		vehicle("1", "L1", "101N", inTransit, t0.Add(5*time.Minute)),
		tripUpdate("1", "L1", call{"101N", t0.Add(5 * time.Minute)}),
	), t0)

	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Errorf("got %d entities, want 0", got)
	}
}

func TestFeedFiltersByRoute(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	tracker.Apply("ace", message(t0, vehicle("2", "X1", "104N", stopped, t0)), t0)

	all := tracker.Feed(t0)
	if len(all.Entity) != 2 {
		t.Fatalf("got %d entities, want 2", len(all.Entity))
	}
	if got := all.Header.GetTimestamp(); got != uint64(t0.Unix()) {
		t.Errorf("header timestamp %d, want the request time %d", got, t0.Unix())
	}
}

func TestApplyReplacesFeedSnapshot(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	tracker.Apply("1234567", message(t0.Add(15*time.Second)), t0.Add(15*time.Second))
	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Errorf("got %d entities after the trip left the feed, want 0", got)
	}
}

func TestResetForgetsEveryTrain(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	tracker.Reset()
	if got := len(tracker.Feed(t0).Entity); got != 0 {
		t.Errorf("got %d entities after a reset, want 0", got)
	}
	if got := len(tracker.Status()); got != 0 {
		t.Errorf("got %d feeds reported after a reset, want 0", got)
	}
}

func TestTrainSeenLeavingStartsFromTheStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "102N", stopped, t0),
		tripUpdate("1", "L1", call{"102N", t0.Add(5 * time.Second)}, call{"103N", t0.Add(100 * time.Second)}),
	), t0)
	later := t0.Add(12 * time.Second)
	tracker.Apply("1234567", message(later,
		vehicle("1", "L1", "103N", inTransit, later),
		tripUpdate("1", "L1", call{"103N", later.Add(100 * time.Second)}),
	), later)

	// The schedule alone would have put it 20 s into a 120 s run.
	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 0, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(50*time.Second))), data, "102N", 500, inTransit, "103N")
}

func TestTrainWhoseStopDropsOutOfItsTripUpdateStartsFromTheStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0,
		vehicle("1", "L1", "102N", stopped, t0.Add(-10*time.Second)),
		tripUpdate("1", "L1", call{"102N", t0.Add(-10 * time.Second)}, call{"103N", t0.Add(50 * time.Second)}),
	), t0)
	later := t0.Add(15 * time.Second)
	tracker.Apply("ace", message(later,
		vehicle("1", "L1", "102N", stopped, t0.Add(-10*time.Second)),
		tripUpdate("1", "L1", call{"103N", later.Add(50 * time.Second)}),
	), later)

	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 0, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(25*time.Second))), data, "102N", 500, inTransit, "103N")
}

func TestTrainEasesOutOfTheStop(t *testing.T) {
	data := network(t)
	tracker := New(data, Config{MaxAge: 10 * time.Minute, Ramp: 20 * time.Second})
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "102N", stopped, t0)), t0)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "103N", inTransit, t0),
		tripUpdate("1", "L1", call{"103N", t0.Add(120 * time.Second)}),
	), t0)

	// Cruising at 1000 / (120 - 20) = 10 m/s, reached after 20 s.
	checkPlaced(t, only(t, tracker.Feed(t0.Add(10*time.Second))), data, "102N", 25, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(t0.Add(60*time.Second))), data, "102N", 500, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(t0.Add(110*time.Second))), data, "103N", 25, inTransit, "103N")
}

func TestBearingFollowsTheTrack(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "103N", inTransit, t0),
		tripUpdate("1", "L1", call{"103N", t0.Add(60 * time.Second)}),
		vehicle("2", "X1", "104N", stopped, t0),
	), t0)

	for _, e := range tracker.Feed(t0).Entity {
		if e.Vehicle.Position.Bearing == nil || e.Vehicle.Position.GetBearing() != 0 {
			t.Errorf("%s: bearing %v, want 0 (due north)", e.Vehicle.Trip.GetTripId(), e.Vehicle.Position.Bearing)
		}
	}
}

func TestSlippingArrivalNeverMovesTheTrainBackwards(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "103N", inTransit, t0),
		tripUpdate("1", "L1", call{"103N", t0.Add(60 * time.Second)}),
	), t0)
	later := t0.Add(30 * time.Second)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 750, inTransit, "103N")

	tracker.Apply("1234567", message(later,
		vehicle("1", "L1", "103N", inTransit, later),
		tripUpdate("1", "L1", call{"103N", later.Add(250 * time.Second)}),
	), later)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 750, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(125*time.Second))), data, "102N", 875, inTransit, "103N")
}

func TestEarlyArrivalIsRunOutNotJumped(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "103N", inTransit, t0),
		tripUpdate("1", "L1", call{"103N", t0.Add(60 * time.Second)}),
	), t0)
	later := t0.Add(15 * time.Second)
	arrived := message(later,
		vehicle("1", "L1", "103N", stopped, later),
		tripUpdate("1", "L1", call{"103N", later.Add(30 * time.Second)}),
	)
	tracker.Apply("1234567", arrived, later)

	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 625, incoming, "103N")
	tracker.Apply("1234567", arrived, later.Add(10*time.Second))
	checkPlaced(t, only(t, tracker.Feed(later.Add(10*time.Second))), data, "102N", 625+10*maxSpeed, incoming, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(20*time.Second))), data, "103N", 0, stopped, "103N")
}

func TestMissedHopIsRunNotJumped(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "102N", stopped, t0)), t0)
	later := t0.Add(60 * time.Second)
	tracker.Apply("ace", message(later, vehicle("1", "L1", "103N", stopped, later)), later)

	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 0, incoming, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(20*time.Second))), data, "102N", 20*maxSpeed, incoming, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(40*time.Second))), data, "103N", 0, stopped, "103N")
}

func TestTrainThatLeftAStopBeforeBeingDrawnThereStartsTheNextHop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "102N", stopped, t0)), t0)
	later := t0.Add(20 * time.Second)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "101N", 20*maxSpeed, incoming, "102N")

	tracker.Apply("ace", message(later,
		vehicle("1", "L1", "102N", stopped, t0),
		tripUpdate("1", "L1", call{"103N", later.Add(60 * time.Second)}),
	), later)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 0, inTransit, "103N")
	checkPlaced(t, only(t, tracker.Feed(later.Add(60*time.Second))), data, "103N", 0, inTransit, "103N")
}

func TestTrainReportedAtAStopBeyondItsHopIsDrawnThere(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "102N", inTransit, t0),
		tripUpdate("1", "L1", call{"102N", t0.Add(120 * time.Second)}),
	), t0)
	later := t0.Add(60 * time.Second)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "101N", 500, inTransit, "102N")

	tracker.Apply("1234567", message(later,
		vehicle("1", "L1", "103N", stopped, later),
		tripUpdate("1", "L1", call{"103N", later}),
	), later)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "103N", 0, stopped, "103N")
}

func TestTrainThatSkippedStopsIsDrawnOnTheHopIntoItsNextStop(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("1234567", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	later := t0.Add(5 * time.Minute)
	tracker.Apply("1234567", message(later,
		vehicle("1", "L1", "104N", inTransit, later),
		tripUpdate("1", "L1", call{"104N", later.Add(60 * time.Second)}),
	), later)

	checkPlaced(t, only(t, tracker.Feed(later)), data, "103N", 500, inTransit, "104N")
}

func TestTrainIsDrawnFromTheStopTheFeedSaysItLeft(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "101N", stopped, t0)), t0)
	later := t0.Add(3 * time.Minute)
	tracker.Apply("ace", message(later,
		vehicle("1", "L1", "102N", stopped, later),
		tripUpdate("1", "L1", call{"103N", later.Add(60 * time.Second)}),
	), later)

	// Expresses run 101N to 103N without stopping, but this train is past 102N.
	checkPlaced(t, only(t, tracker.Feed(later)), data, "102N", 500, inTransit, "103N")
}

func TestChangeOfCourseStartsAfresh(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "104N", stopped, t0)), t0)
	tracker.Apply("ace", message(t0, vehicle("1", "L1", "201N", stopped, t0)), t0)

	checkPlaced(t, only(t, tracker.Feed(t0)), data, "201N", 0, stopped, "201N")
}

func TestTrainAtTheEndOfItsTripIsLeftOutAfterThreeMinutes(t *testing.T) {
	tracker := New(network(t), config)
	arrived := t0.Add(-30 * time.Second)
	finished := []*gtfs.FeedEntity{
		vehicle("1", "L1", "104N", stopped, arrived),
		tripUpdate("1", "L1"),
		vehicle("1", "L2", "104N", stopped, arrived),
		tripUpdate("1", "L2", call{"104N", arrived}),
		vehicle("2", "X1", "103N", stopped, arrived),
		tripUpdate("2", "X1", call{"103N", arrived}, call{"104N", t0.Add(5 * time.Minute)}),
	}
	tracker.Apply("1234567", message(t0, finished...), t0)
	if got := len(tracker.Feed(t0).Entity); got != 3 {
		t.Fatalf("got %d entities 30 s after arriving, want 3", got)
	}

	later := t0.Add(3 * time.Minute)
	tracker.Apply("1234567", message(later, finished...), later)
	if v := only(t, tracker.Feed(later)); v.Trip.GetTripId() != "X1" {
		t.Errorf("kept trip %s, want only X1, which has a stop left", v.Trip.GetTripId())
	}
}

func TestStaleTrainsAreLeftOut(t *testing.T) {
	tracker := New(network(t), config)
	tracker.Apply("1234567", message(t0,
		vehicle("1", "L1", "101N", stopped, t0.Add(-9*time.Minute)),
		vehicle("2", "X1", "104N", stopped, t0.Add(-11*time.Minute)),
	), t0)

	if v := only(t, tracker.Feed(t0)); v.Trip.GetTripId() != "L1" {
		t.Errorf("kept trip %s, want only L1", v.Trip.GetTripId())
	}
	if got := len(tracker.Feed(t0.Add(2 * time.Minute)).Entity); got != 0 {
		t.Errorf("got %d entities once both trains are over 10 minutes old, want 0", got)
	}
}
