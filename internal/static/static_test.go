package static

import (
	"math"
	"testing"
	"time"

	"github.com/jdamcd/gtfs-position-interpolator/internal/geo"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static/statictest"
)

func network(t *testing.T) *Data {
	t.Helper()
	data, err := Parse(statictest.Zip(t, statictest.Network()))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func checkSegment(t *testing.T, s *Segment, length float64, runTime time.Duration) {
	t.Helper()
	if s == nil {
		t.Fatal("no segment")
	}
	if math.Abs(s.Line.Length()-length) > 2 {
		t.Errorf("length %.1f, want %.1f", s.Line.Length(), length)
	}
	if s.RunTime != runTime {
		t.Errorf("run time %v, want %v", s.RunTime, runTime)
	}
}

func TestParseStops(t *testing.T) {
	data := network(t)
	if got, want := data.Stops["101N"], (geo.Point{Lat: 40.7, Lon: -73.99}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseMissingColumn(t *testing.T) {
	files := statictest.Network()
	files["stops.txt"] = "stop_id,stop_lat\n101N,40.7\n"
	if _, err := Parse(statictest.Zip(t, files)); err == nil {
		t.Fatal("expected an error for a missing stop_lon column")
	}
}

func TestSegmentsFollowTheShape(t *testing.T) {
	data := network(t)
	local := data.Segment("101N", "102N")
	checkSegment(t, local, 1000, 120*time.Second)
	if got := len(local.Line.Points()); got != 3 {
		t.Errorf("local segment has %d points, want the 3 shape points between the stops", got)
	}
	checkSegment(t, data.Segment("101N", "103N"), 2000, 200*time.Second)
}

func TestSegmentChainsUnscheduledPairs(t *testing.T) {
	checkSegment(t, network(t).Segment("102N", "104N"), 2000, 240*time.Second)
}

func TestHopIsOnlyBetweenStopsATrainRunsWithoutStopping(t *testing.T) {
	data := network(t)
	for pair, want := range map[[2]string]bool{
		{"101N", "102N"}: true,
		{"101N", "103N"}: true,
		{"102N", "104N"}: false,
		{"101N", "201N"}: false,
	} {
		if got := data.Hop(pair[0], pair[1]) != nil; got != want {
			t.Errorf("hop from %s to %s: %v, want %v", pair[0], pair[1], got, want)
		}
	}
}

func TestSegmentFallsBackToStraightLine(t *testing.T) {
	data := network(t)
	s := data.Segment("101N", "201N")
	checkSegment(t, s, 5000, 0)
	if got := len(s.Line.Points()); got != 2 {
		t.Errorf("fallback segment has %d points, want 2", got)
	}
	if data.Segment("101N", "999N") != nil {
		t.Error("expected no segment to an unknown stop")
	}
}

func TestPredecessorPrefersTheRoute(t *testing.T) {
	data := network(t)
	for route, want := range map[string]string{"1": "102N", "2": "101N", "9": "102N"} {
		if got := data.Predecessor("103N", route); got != want {
			t.Errorf("route %s: predecessor of 103N = %q, want %q", route, got, want)
		}
	}
	if got := data.Predecessor("101N", "1"); got != "" {
		t.Errorf("predecessor of the first stop = %q, want none", got)
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]int{"08:02:30": 28950, "24:10:00": 87000, "": -1, "8:02": -1} {
		if got := parseClock(in); got != want {
			t.Errorf("parseClock(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseWithoutShapes(t *testing.T) {
	files := statictest.Network()
	delete(files, "shapes.txt")
	files["trips.txt"] = "route_id,trip_id,service_id\n1,L1,Weekday\n1,L2,Weekday\n2,X1,Weekday\n1,L3,Weekday\n"
	data, err := Parse(statictest.Zip(t, files))
	if err != nil {
		t.Fatal(err)
	}
	s := data.Segment("101N", "102N")
	checkSegment(t, s, 1000, 120*time.Second)
	if got := len(s.Line.Points()); got != 2 {
		t.Errorf("segment has %d points, want a straight line between the stops", got)
	}
}

func TestParseSkipsStopsWithoutCoordinates(t *testing.T) {
	files := statictest.Network()
	files["stops.txt"] += "E1,Entrance,,\n"
	data, err := Parse(statictest.Zip(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := data.Stops["E1"]; ok {
		t.Error("a stop with no coordinates should be left out")
	}
}

func TestSchedule(t *testing.T) {
	data := network(t)
	got := data.Schedule("L1")
	if len(got) != 4 {
		t.Fatalf("got %d stop times, want 4", len(got))
	}
	if want := (StopTime{Sequence: 2, Stop: "102N", Arrival: 28920, Departure: 28950}); got[1] != want {
		t.Errorf("second call %+v, want %+v", got[1], want)
	}
	if data.Schedule("nope") != nil {
		t.Error("expected no schedule for an unknown trip")
	}
}

func TestLocation(t *testing.T) {
	if got := network(t).Location; got == nil || got.String() != "America/New_York" {
		t.Errorf("location %v, want America/New_York", got)
	}

	files := statictest.Network()
	delete(files, "agency.txt")
	data, err := Parse(statictest.Zip(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if data.Location != nil {
		t.Errorf("location %v without agency.txt, want none", data.Location)
	}

	for name, agency := range map[string]string{
		"an unknown timezone": "agency_name,agency_timezone\nTest Transit,Nowhere/Land\n",
		"no timezone column":  "agency_name\nTest Transit\n",
	} {
		files["agency.txt"] = agency
		data, err := Parse(statictest.Zip(t, files))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if data.Location != nil {
			t.Errorf("%s: location %v, want none", name, data.Location)
		}
	}

	files["agency.txt"] = "agency_name,agency_timezone\nTest Transit, America/Chicago \n"
	data, err = Parse(statictest.Zip(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if got := data.Location; got == nil || got.String() != "America/Chicago" {
		t.Errorf("location %v from a padded value, want America/Chicago", got)
	}
}
