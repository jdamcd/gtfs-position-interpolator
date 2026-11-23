package geo

import (
	"math"
	"slices"
)

const earthRadius = 6371008.8

type Point struct {
	Lat, Lon float64
}

// Equirectangular approximation: accurate to well under a metre across a city.
func offset(a, b Point) (east, north float64) {
	east = radians(b.Lon-a.Lon) * math.Cos(radians((a.Lat+b.Lat)/2)) * earthRadius
	north = radians(b.Lat-a.Lat) * earthRadius
	return east, north
}

func Distance(a, b Point) float64 {
	return math.Hypot(offset(a, b))
}

func Bearing(a, b Point) float64 {
	east, north := offset(a, b)
	deg := math.Atan2(east, north) * 180 / math.Pi
	if deg < 0 {
		deg += 360
	}
	return deg
}

type Line struct {
	points []Point
	cum    []float64
}

func NewLine(points []Point) Line {
	l := Line{points: points, cum: make([]float64, len(points))}
	for i := 1; i < len(points); i++ {
		l.cum[i] = l.cum[i-1] + Distance(points[i-1], points[i])
	}
	return l
}

func (l Line) Points() []Point {
	return l.points
}

func (l Line) Length() float64 {
	if len(l.cum) == 0 {
		return 0
	}
	return l.cum[len(l.cum)-1]
}

func (l Line) At(dist float64) Point {
	p, _ := l.locate(dist)
	return p
}

// BearingAt is in compass degrees, and meaningless for a zero-length line.
func (l Line) BearingAt(dist float64) float64 {
	_, i := l.locate(dist)
	lo, hi := i, i+1
	for hi < len(l.points)-1 && l.cum[hi] == l.cum[lo] {
		hi++
	}
	for lo > 0 && l.cum[hi] == l.cum[lo] {
		lo--
	}
	if hi >= len(l.points) {
		return 0
	}
	return Bearing(l.points[lo], l.points[hi])
}

func (l Line) locate(dist float64) (Point, int) {
	n := len(l.points)
	if n == 0 {
		return Point{}, 0
	}
	if n == 1 || dist <= 0 {
		return l.points[0], 0
	}
	if dist >= l.Length() {
		return l.points[n-1], n - 2
	}
	i, found := slices.BinarySearch(l.cum, dist)
	if found {
		return l.points[i], min(i, n-2)
	}
	i--
	t := (dist - l.cum[i]) / (l.cum[i+1] - l.cum[i])
	a, b := l.points[i], l.points[i+1]
	return Point{Lat: a.Lat + t*(b.Lat-a.Lat), Lon: a.Lon + t*(b.Lon-a.Lon)}, i
}

// Project finds the point closest to p between from and to metres along the
// line. It returns the distance along, and how far p is from that point.
func (l Line) Project(p Point, from, to float64) (dist, away float64) {
	from = math.Max(from, 0)
	to = math.Min(to, l.Length())
	dist, away = from, Distance(p, l.At(from))
	for i := 0; i+1 < len(l.points); i++ {
		if l.cum[i+1] < from || l.cum[i] > to {
			continue
		}
		a, b := l.points[i], l.points[i+1]
		bx, by := offset(a, b)
		px, py := offset(a, p)
		t := 0.0
		if length := bx*bx + by*by; length > 0 {
			t = math.Max(0, math.Min(1, (px*bx+py*by)/length))
		}
		d := math.Max(from, math.Min(to, l.cum[i]+t*(l.cum[i+1]-l.cum[i])))
		if candidate := Distance(p, l.At(d)); candidate < away {
			dist, away = d, candidate
		}
	}
	return dist, away
}

func (l Line) Slice(from, to float64) Line {
	from = math.Max(from, 0)
	to = math.Max(from, math.Min(to, l.Length()))
	points := []Point{l.At(from)}
	for i, d := range l.cum {
		if d > from && d < to {
			points = append(points, l.points[i])
		}
	}
	return NewLine(append(points, l.At(to)))
}

func Join(lines ...Line) Line {
	var points []Point
	for _, l := range lines {
		for _, p := range l.points {
			if len(points) == 0 || points[len(points)-1] != p {
				points = append(points, p)
			}
		}
	}
	return NewLine(points)
}

func radians(deg float64) float64 {
	return deg * math.Pi / 180
}
