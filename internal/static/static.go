package static

import (
	"archive/zip"
	"bytes"
	"cmp"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdamcd/gtfs-position-interpolator/internal/geo"
)

const (
	// A shape further than this from a stop is not trusted for the hops either
	// side of it.
	maxStopOffset = 300.0
	maxChainHops  = 10
	// Track straying further than this beyond its end stops has doubled back.
	maxOvershoot = 30.0
)

// Segment is the track between two stops that some train runs nonstop.
type Segment struct {
	Line geo.Line
	// RunTime is the median scheduled time, or zero if no trip runs the pair
	// directly.
	RunTime time.Duration
}

type hop struct {
	from, to string
}

type trip struct {
	route, shape string
}

type origin struct {
	stop   string
	trips  int
	routes map[string]int
}

// Times are in seconds from the start of the service day, or -1 for none.
type StopTime struct {
	Sequence           uint32
	Stop               string
	Arrival, Departure int
}

type Data struct {
	Stops map[string]geo.Point
	// Location is the agency's timezone, or nil if the feed gives none.
	Location *time.Location

	stopIDs   []string
	schedules map[string][]stopTime
	trips     map[string]trip

	segments map[hop]*Segment
	bearings map[string]float64
	next     map[string][]string
	previous map[string][]origin

	mu      sync.Mutex
	derived map[hop]*Segment
}

// Segment chains shorter segments for a pair no trip runs directly, and
// failing that draws a straight line. It returns nil for an unknown stop.
func (d *Data) Segment(from, to string) *Segment {
	if s := d.segments[hop{from, to}]; s != nil {
		return s
	}
	a, okA := d.Stops[from]
	b, okB := d.Stops[to]
	if !okA || !okB || from == to {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.derived[hop{from, to}]; s != nil {
		return s
	}
	s := d.chain(from, to)
	if s == nil {
		s = &Segment{Line: geo.NewLine([]geo.Point{a, b})}
	}
	d.derived[hop{from, to}] = s
	return s
}

// Hop returns the track some train runs nonstop between two stops, or nil.
func (d *Data) Hop(from, to string) *Segment {
	return d.segments[hop{from, to}]
}

func (d *Data) chain(from, to string) *Segment {
	cameFrom := map[string]string{from: ""}
	frontier := []string{from}
	for depth := 0; depth < maxChainHops && len(frontier) > 0; depth++ {
		var reached []string
		for _, stop := range frontier {
			for _, n := range d.next[stop] {
				if _, seen := cameFrom[n]; seen {
					continue
				}
				cameFrom[n] = stop
				reached = append(reached, n)
			}
		}
		if _, found := cameFrom[to]; found {
			var lines []geo.Line
			var runTime time.Duration
			for stop := to; stop != from; stop = cameFrom[stop] {
				s := d.segments[hop{cameFrom[stop], stop}]
				lines = append([]geo.Line{s.Line}, lines...)
				runTime += s.RunTime
			}
			return &Segment{Line: geo.Join(lines...), RunTime: runTime}
		}
		frontier = reached
	}
	return nil
}

func (d *Data) Bearing(stop string) (float64, bool) {
	b, ok := d.bearings[stop]
	return b, ok
}

// Predecessor guesses the stop a train came from, preferring those its route
// is scheduled to come from.
func (d *Data) Predecessor(to, route string) string {
	best, bestTrips := "", 0
	for _, o := range d.previous[to] {
		if n := o.routes[route]; n > bestTrips {
			best, bestTrips = o.stop, n
		}
	}
	if best == "" && len(d.previous[to]) > 0 {
		return d.previous[to][0].stop
	}
	return best
}

// Schedule returns a trip's calls in stop sequence order, or nil for an
// unknown trip.
func (d *Data) Schedule(tripID string) []StopTime {
	times := d.schedules[tripID]
	if times == nil {
		return nil
	}
	out := make([]StopTime, len(times))
	for i, st := range times {
		out[i] = StopTime{Sequence: st.sequence, Stop: d.stopIDs[st.stop], Arrival: int(st.arrival), Departure: int(st.departure)}
	}
	return out
}

func (d *Data) Route(tripID string) string {
	return d.trips[tripID].route
}

// PreviousStop finds a call by stop sequence if given, otherwise by stop, and
// returns the stop called at before it: "" for the first call. It reports
// false if the call is not in the schedule.
func (d *Data) PreviousStop(tripID, stop string, sequence *uint32) (string, bool) {
	times := d.schedules[tripID]
	for i, st := range times {
		if sequence != nil && st.sequence != *sequence {
			continue
		}
		if sequence == nil && d.stopIDs[st.stop] != stop {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if prev := d.stopIDs[times[j].stop]; d.HasStop(prev) {
				return prev, true
			}
		}
		return "", true
	}
	return "", false
}

func (d *Data) HasStop(id string) bool {
	_, ok := d.Stops[id]
	return ok
}

// Every stop time in the feed is kept, so stops are indexes into
// Data.stopIDs to hold each one to 16 bytes.
type stopTime struct {
	sequence           uint32
	stop               uint32
	arrival, departure int32
}

type hopBuild struct {
	runTimes []int
	routes   map[string]int
	line     geo.Line
	away     float64
	hasLine  bool
}

func Parse(zipped []byte) (*Data, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, err
	}

	stops := map[string]geo.Point{}
	err = readTable(zr, "stops.txt", []string{"stop_id", "stop_lat", "stop_lon"}, func(row []string) error {
		// Entrances and other places inside a station may have no position.
		if strings.TrimSpace(row[1]) == "" && strings.TrimSpace(row[2]) == "" {
			return nil
		}
		p, err := parsePoint(row[1], row[2])
		stops[row[0]] = p
		return err
	})
	if err != nil {
		return nil, err
	}

	// Only trip updates giving delays need the timezone, so a feed without a
	// readable one still loads.
	var location *time.Location
	err = readTable(zr, "agency.txt", []string{"agency_timezone?"}, func(row []string) error {
		zone := strings.TrimSpace(row[0])
		if location != nil || zone == "" {
			return nil
		}
		loaded, err := time.LoadLocation(zone)
		if err != nil {
			slog.Warn("static GTFS has an unknown agency timezone", "timezone", zone)
			return nil
		}
		location = loaded
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	trips := map[string]trip{}
	err = readTable(zr, "trips.txt", []string{"trip_id", "route_id", "shape_id?"}, func(row []string) error {
		trips[row[0]] = trip{route: row[1], shape: row[2]}
		return nil
	})
	if err != nil {
		return nil, err
	}

	type shapePoint struct {
		sequence int
		point    geo.Point
	}
	shapePoints := map[string][]shapePoint{}
	err = readTable(zr, "shapes.txt", []string{"shape_id", "shape_pt_sequence", "shape_pt_lat", "shape_pt_lon"}, func(row []string) error {
		seq, err := strconv.Atoi(row[1])
		if err != nil {
			return err
		}
		p, err := parsePoint(row[2], row[3])
		shapePoints[row[0]] = append(shapePoints[row[0]], shapePoint{seq, p})
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	shapes := map[string]geo.Line{}
	for id, sp := range shapePoints {
		sort.Slice(sp, func(i, j int) bool { return sp[i].sequence < sp[j].sequence })
		points := make([]geo.Point, len(sp))
		for i := range sp {
			points[i] = sp[i].point
		}
		shapes[id] = geo.NewLine(points)
	}

	var stopIDs []string
	stopIndex := map[string]uint32{}
	stopTimes := map[string][]stopTime{}
	err = readTable(zr, "stop_times.txt", []string{"trip_id", "stop_id", "arrival_time?", "departure_time?", "stop_sequence"}, func(row []string) error {
		seq, err := strconv.ParseUint(row[4], 10, 32)
		if err != nil {
			return err
		}
		stop, ok := stopIndex[row[1]]
		if !ok {
			stop = uint32(len(stopIDs))
			stopIndex[row[1]] = stop
			stopIDs = append(stopIDs, row[1])
		}
		stopTimes[row[0]] = append(stopTimes[row[0]], stopTime{
			sequence:  uint32(seq),
			stop:      stop,
			arrival:   int32(parseClock(row[2])),
			departure: int32(parseClock(row[3])),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	tripIDs := make([]string, 0, len(stopTimes))
	for id := range stopTimes {
		tripIDs = append(tripIDs, id)
	}
	sort.Strings(tripIDs)

	hops := map[hop]*hopBuild{}
	patterns := map[string]bool{}
	for _, id := range tripIDs {
		times := slices.Clone(stopTimes[id])
		sort.Slice(times, func(i, j int) bool { return times[i].sequence < times[j].sequence })
		stopTimes[id] = times
		t := trips[id]
		ids := make([]string, len(times))
		for i, st := range times {
			ids[i] = stopIDs[st.stop]
			if i == 0 {
				continue
			}
			h := hop{ids[i-1], ids[i]}
			b := hops[h]
			if b == nil {
				b = &hopBuild{routes: map[string]int{}}
				hops[h] = b
			}
			b.routes[t.route]++
			if dep, arr := times[i-1].departure, st.arrival; dep >= 0 && arr >= dep {
				b.runTimes = append(b.runTimes, int(arr-dep))
			}
		}
		pattern := t.shape + "|" + strings.Join(ids, ",")
		shape, ok := shapes[t.shape]
		if patterns[pattern] || !ok {
			continue
		}
		patterns[pattern] = true
		dists, aways := projectStops(shape, ids, stops)
		for i := 1; i < len(ids); i++ {
			away := aways[i-1] + aways[i]
			b := hops[hop{ids[i-1], ids[i]}]
			if aways[i-1] > maxStopOffset || aways[i] > maxStopOffset || dists[i] <= dists[i-1] {
				continue
			}
			line := shape.Slice(dists[i-1], dists[i])
			if overshoots(line, stops[ids[i-1]], stops[ids[i]]) {
				continue
			}
			if !b.hasLine || away < b.away {
				b.line, b.away, b.hasLine = line, away, true
			}
		}
	}

	d := &Data{
		Stops:     stops,
		Location:  location,
		stopIDs:   stopIDs,
		schedules: stopTimes,
		trips:     trips,
		segments:  map[hop]*Segment{},
		bearings:  map[string]float64{},
		next:      map[string][]string{},
		previous:  map[string][]origin{},
		derived:   map[hop]*Segment{},
	}
	hopKeys := slices.SortedFunc(maps.Keys(hops), func(a, b hop) int {
		return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(a.to, b.to))
	})
	for _, h := range hopKeys {
		b := hops[h]
		a, okA := stops[h.from]
		z, okZ := stops[h.to]
		if !okA || !okZ {
			continue
		}
		line := b.line
		if !b.hasLine {
			line = geo.NewLine([]geo.Point{a, z})
		}
		d.segments[h] = &Segment{Line: line, RunTime: median(b.runTimes)}
		d.next[h.from] = append(d.next[h.from], h.to)
		trips := 0
		for _, n := range b.routes {
			trips += n
		}
		d.previous[h.to] = append(d.previous[h.to], origin{stop: h.from, trips: trips, routes: b.routes})
	}
	for _, list := range d.next {
		sort.Strings(list)
	}
	for _, list := range d.previous {
		sort.Slice(list, func(i, j int) bool {
			if list[i].trips != list[j].trips {
				return list[i].trips > list[j].trips
			}
			return list[i].stop < list[j].stop
		})
	}
	for stop, origins := range d.previous {
		if line := d.segments[hop{origins[0].stop, stop}].Line; line.Length() > 0 {
			d.bearings[stop] = line.BearingAt(line.Length())
		}
	}
	for stop, nexts := range d.next {
		if _, ok := d.bearings[stop]; ok {
			continue
		}
		if line := d.segments[hop{stop, nexts[0]}].Line; line.Length() > 0 {
			d.bearings[stop] = line.BearingAt(0)
		}
	}
	return d, nil
}

// projectStops places each stop on the shape in travel order. Each search
// starts where the last stop landed and is bounded by the distance between
// the two, so a shape passing a place twice can't capture a stop on the
// wrong pass.
func projectStops(shape geo.Line, ids []string, stops map[string]geo.Point) (dists, aways []float64) {
	dists = make([]float64, len(ids))
	aways = make([]float64, len(ids))
	along := 0.0
	var last geo.Point
	placed := false
	for i, id := range ids {
		p, ok := stops[id]
		if !ok {
			dists[i], aways[i] = along, maxStopOffset*2
			continue
		}
		limit := shape.Length()
		if placed {
			limit = along + 3*geo.Distance(last, p) + 500
		}
		along, aways[i] = shape.Project(p, along, limit)
		dists[i] = along
		last, placed = p, true
	}
	return dists, aways
}

// overshoots reports whether a line between two stops strays further from
// either than they are apart.
func overshoots(line geo.Line, from, to geo.Point) bool {
	limit := geo.Distance(from, to) + maxOvershoot
	for _, p := range line.Points() {
		if geo.Distance(p, from) > limit || geo.Distance(p, to) > limit {
			return true
		}
	}
	return false
}

func median(seconds []int) time.Duration {
	if len(seconds) == 0 {
		return 0
	}
	sort.Ints(seconds)
	return time.Duration(seconds[len(seconds)/2]) * time.Second
}

func parsePoint(lat, lon string) (geo.Point, error) {
	la, err := strconv.ParseFloat(strings.TrimSpace(lat), 64)
	if err != nil {
		return geo.Point{}, err
	}
	lo, err := strconv.ParseFloat(strings.TrimSpace(lon), 64)
	if err != nil {
		return geo.Point{}, err
	}
	return geo.Point{Lat: la, Lon: lo}, nil
}

// parseClock reads HH:MM:SS, which may run past 24:00:00, as seconds, or -1
// if blank or malformed.
func parseClock(s string) int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return -1
	}
	total := 0
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return -1
		}
		total = total*60 + n
	}
	return total
}

// readTable calls each with the named columns of every row. A column named
// with a trailing "?" may be absent, and reads as empty.
func readTable(zr *zip.Reader, name string, columns []string, each func(row []string) error) error {
	f, err := zr.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	index := make([]int, len(columns))
	for i, want := range columns {
		want, optional := strings.CutSuffix(want, "?")
		index[i] = -1
		for j, got := range header {
			if strings.TrimPrefix(strings.TrimSpace(got), "\ufeff") == want {
				index[i] = j
			}
		}
		if index[i] < 0 && !optional {
			return fmt.Errorf("%s: missing column %s", name, want)
		}
	}
	row := make([]string, len(columns))
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for i, j := range index {
			if j < 0 {
				continue
			}
			if j >= len(rec) {
				return fmt.Errorf("%s: short row", name)
			}
			row[i] = strings.Clone(rec[j])
		}
		if err := each(row); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
}
