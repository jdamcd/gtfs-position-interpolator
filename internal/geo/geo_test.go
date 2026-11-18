package geo

import (
	"math"
	"testing"
)

// An L-shaped line: 1000 m north, then 1000 m east.
func corner() Line {
	origin := Point{Lat: 40.75, Lon: -73.99}
	north := Point{Lat: origin.Lat + degrees(1000/earthRadius), Lon: origin.Lon}
	east := Point{Lat: north.Lat, Lon: north.Lon + degrees(1000/earthRadius)/math.Cos(radians(north.Lat))}
	return NewLine([]Point{origin, north, east})
}

func degrees(rad float64) float64 {
	return rad * 180 / math.Pi
}

func near(t *testing.T, name string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %.3f, want %.3f (±%.3f)", name, got, want, tolerance)
	}
}

func TestLineLengthAndAt(t *testing.T) {
	l := corner()
	near(t, "length", l.Length(), 2000, 0.5)
	near(t, "distance from start to At(500)", Distance(l.Points()[0], l.At(500)), 500, 0.5)
	near(t, "distance from corner to At(1500)", Distance(l.Points()[1], l.At(1500)), 500, 0.5)
	if l.At(-10) != l.Points()[0] || l.At(5000) != l.Points()[2] {
		t.Error("At should clamp to the ends of the line")
	}
}

func TestBearingAt(t *testing.T) {
	l := corner()
	near(t, "bearing on the first leg", l.BearingAt(500), 0, 0.01)
	near(t, "bearing on the second leg", l.BearingAt(1500), 90, 0.01)
	near(t, "bearing at the end", l.BearingAt(2000), 90, 0.01)
}

func TestBearingAtSkipsRepeatedPoints(t *testing.T) {
	c := corner().Points()
	l := NewLine([]Point{c[0], c[0], c[1], c[1]})
	near(t, "bearing at the start", l.BearingAt(0), 0, 0.01)
	near(t, "bearing at the end", l.BearingAt(l.Length()), 0, 0.01)
}

func TestProject(t *testing.T) {
	l := corner()
	beside := Point{Lat: l.At(400).Lat, Lon: l.At(400).Lon + 0.0005}
	dist, away := l.Project(beside, 0, l.Length())
	near(t, "distance along", dist, 400, 0.5)
	near(t, "distance away", away, Distance(beside, l.At(400)), 0.5)

	dist, _ = l.Project(beside, 900, l.Length())
	near(t, "distance along when the window starts later", dist, 900, 0.5)
}

func TestSliceAndJoin(t *testing.T) {
	l := corner()
	s := l.Slice(500, 1500)
	near(t, "slice length", s.Length(), 1000, 0.5)
	if len(s.Points()) != 3 {
		t.Errorf("slice has %d points, want 3 (start, corner, end)", len(s.Points()))
	}
	joined := Join(l.Slice(0, 500), s, l.Slice(1500, 2000))
	near(t, "joined length", joined.Length(), 2000, 0.5)
	if len(joined.Points()) != 5 {
		t.Errorf("joined line has %d points, want 5 with shared ends merged", len(joined.Points()))
	}
}
