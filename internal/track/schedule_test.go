package track

import (
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// Trip L1 in the test network leaves 101N at 08:00:00, calls at 102N from
// 08:02:00 to 08:02:30, and reaches 103N at 08:04:30 and 104N at 08:06:30.
func clock(hour, minute, second int) time.Time {
	return time.Date(2026, 11, 16, hour, minute, second, 0, newYork)
}

func delayed(stop string, seconds int32) *gtfs.TripUpdate_StopTimeUpdate {
	event := &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(seconds)}
	return &gtfs.TripUpdate_StopTimeUpdate{StopId: proto.String(stop), Arrival: event, Departure: event}
}

func delayUpdate(id, date string, updates ...*gtfs.TripUpdate_StopTimeUpdate) *gtfs.FeedEntity {
	descriptor := &gtfs.TripDescriptor{TripId: proto.String(id), RouteId: proto.String("1")}
	if date != "" {
		descriptor.StartDate = proto.String(date)
	}
	return &gtfs.FeedEntity{Id: proto.String("t" + id), TripUpdate: &gtfs.TripUpdate{Trip: descriptor, StopTimeUpdate: updates}}
}

func TestServiceDayOnADayTheClocksChange(t *testing.T) {
	day, ok := serviceDay("20261101", newYork)
	if !ok {
		t.Fatal("no service day")
	}
	got := day.Add(8 * time.Hour)
	if want := time.Date(2026, 11, 1, 8, 0, 0, 0, newYork); !got.Equal(want) {
		t.Errorf("08:00:00 on the day the clocks go back is %v, want %v", got, want)
	}
}

func TestDelaysAreAddedToTheSchedule(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	now := clock(8, 2, 0)
	tracker.Apply("feed", message(now, delayUpdate("L1", "20261116", delayed("101N", 60), delayed("102N", 60), delayed("103N", 60), delayed("104N", 60))), now)

	checkPlaced(t, only(t, tracker.Feed(now)), data, "101N", 500, inTransit, "102N")
}

func TestDelayCarriesToStopsWithNoUpdate(t *testing.T) {
	data := network(t)
	for name, update := range map[string]*gtfs.TripUpdate_StopTimeUpdate{
		"the first stop": delayed("101N", 60),
		"a later stop":   delayed("103N", 60),
		"a stop given only by sequence": {
			StopSequence: proto.Uint32(3),
			Arrival:      &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(60)},
		},
	} {
		tracker := New(data, config)
		now := clock(8, 2, 0)
		tracker.Apply("feed", message(now, delayUpdate("L1", "20261116", update)), now)
		if got := len(tracker.Feed(now).Entity); got != 1 {
			t.Errorf("update for %s: %d vehicles, want 1", name, got)
			continue
		}
		checkPlaced(t, only(t, tracker.Feed(now)), data, "101N", 500, inTransit, "102N")
	}
}

func TestDelayedTripHasNotStartedUntilItsLateDeparture(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	update := delayUpdate("L1", "20261116", delayed("101N", 120))
	early := clock(8, 1, 0)
	tracker.Apply("feed", message(early, update), early)
	if got := len(tracker.Feed(early).Entity); got != 0 {
		t.Fatalf("%d vehicles before the delayed departure, want none", got)
	}
	later := clock(8, 3, 0)
	tracker.Apply("feed", message(later, update), later)
	checkPlaced(t, only(t, tracker.Feed(later)), data, "101N", 500, inTransit, "102N")
}

func TestStandingAtAScheduledDwell(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	now := clock(8, 2, 10)
	tracker.Apply("feed", message(now, delayUpdate("L1", "20261116", delayed("101N", 0))), now)
	checkPlaced(t, only(t, tracker.Feed(now)), data, "102N", 0, stopped, "102N")
}

func TestTripLevelDelay(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	update := delayUpdate("L1", "20261116")
	update.TripUpdate.Delay = proto.Int32(60)
	now := clock(8, 2, 0)
	tracker.Apply("feed", message(now, update), now)
	checkPlaced(t, only(t, tracker.Feed(now)), data, "101N", 500, inTransit, "102N")
}

func TestTimesPastMidnightBelongToTheDayBefore(t *testing.T) {
	data := network(t)
	tracker := New(data, config)
	// Trip L2 leaves 101N at 24:10:00 and reaches 102N at 24:12:00.
	now := time.Date(2026, 11, 17, 0, 11, 0, 0, newYork)
	tracker.Apply("feed", message(now, delayUpdate("L2", "20261116", delayed("101N", 0))), now)
	checkPlaced(t, only(t, tracker.Feed(now)), data, "101N", 500, inTransit, "102N")
}

func TestStartDateIsWorkedOutWhenMissing(t *testing.T) {
	data := network(t)
	for name, c := range map[string]struct {
		trip string
		now  time.Time
	}{
		"a trip today":                         {"L1", clock(8, 1, 0)},
		"a trip that began the evening before": {"L2", time.Date(2026, 11, 17, 0, 11, 0, 0, newYork)},
	} {
		tracker := New(data, config)
		tracker.Apply("feed", message(c.now, delayUpdate(c.trip, "", delayed("101N", 0))), c.now)
		if got := len(tracker.Feed(c.now).Entity); got != 1 {
			t.Errorf("%s: %d vehicles, want 1", name, got)
			continue
		}
		checkPlaced(t, only(t, tracker.Feed(c.now)), data, "101N", 500, inTransit, "102N")
	}
}

func TestDelaysForATripNotInTheScheduleAreLeftOut(t *testing.T) {
	tracker := New(network(t), config)
	now := clock(8, 2, 0)
	tracker.Apply("feed", message(now, delayUpdate("EXTRA", "20261116", delayed("102N", 0))), now)
	if got := len(tracker.Feed(now).Entity); got != 0 {
		t.Errorf("%d vehicles, want none", got)
	}
}
