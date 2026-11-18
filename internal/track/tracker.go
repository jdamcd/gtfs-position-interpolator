package track

import (
	"sort"
	"sync"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/geo"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
)

const (
	// The lettered lines list trains that have not started their trip as
	// in transit, stamped with their future departure time.
	notStartedTolerance = 30 * time.Second

	// Used to time a hop when the schedule has no run time for it.
	fallbackSpeed = 10.0
)

type Config struct {
	// MaxAge is how old a vehicle's upstream timestamp can be before the
	// train is dropped from the feed.
	MaxAge time.Duration
	// Ramp is how long a train takes to reach cruising speed out of a stop,
	// and to brake into the next.
	Ramp time.Duration
}

// A leg is a train's run along a stretch of track to a stop. The anchor is
// the last moment its place and speed on the line were pinned down; from
// there it runs to the end of the line, due at the predicted arrival time.
type leg struct {
	to          string
	line        geo.Line
	eta         time.Time
	ramp        time.Duration
	anchorAt    time.Time
	anchor      float64
	anchorSpeed float64
}

func (l *leg) advance(now time.Time) (distance, speed float64) {
	return advance(l.anchor, l.anchorSpeed, l.line.Length(),
		l.eta.Sub(l.anchorAt).Seconds(), l.ramp.Seconds(), now.Sub(l.anchorAt).Seconds())
}

type train struct {
	trip      *gtfs.TripDescriptor
	vehicle   *gtfs.VehicleDescriptor
	timestamp time.Time
	obs       observation
	// leg is nil when the train is drawn standing at a stop.
	leg *leg
}

type snapshot struct {
	header time.Time
	trains map[string]*train
	// usesVehicles is set once a feed has carried vehicle entities. Those
	// say which trips are running and where, so trip updates without one
	// are trips yet to start.
	usesVehicles bool
}

type Tracker struct {
	config Config
	mu     sync.RWMutex
	static *static.Data
	feeds  map[string]snapshot
}

func New(data *static.Data, config Config) *Tracker {
	return &Tracker{config: config, static: data, feeds: map[string]snapshot{}}
}

// Trains finish the leg they are on along the track they started it with.
func (t *Tracker) SetStatic(data *static.Data) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.static = data
}

type FeedStatus struct {
	// Header is the upstream timestamp of the latest snapshot applied.
	Header   time.Time
	Vehicles int
}

func (t *Tracker) Status() map[string]FeedStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := map[string]FeedStatus{}
	for name, feed := range t.feeds {
		out[name] = FeedStatus{Header: feed.header, Vehicles: len(feed.trains)}
	}
	return out
}

func (t *Tracker) Apply(feed string, msg *gtfs.FeedMessage, now time.Time) {
	updates := map[string]*gtfs.TripUpdate{}
	var vehicles []*gtfs.VehiclePosition
	for _, e := range msg.GetEntity() {
		// Trips are told apart by their ID, so one without is left out.
		if tu := e.GetTripUpdate(); tu.GetTrip().GetTripId() != "" {
			updates[tripKey(tu.GetTrip())] = tu
		}
		if v := e.GetVehicle(); v.GetTrip().GetTripId() != "" {
			vehicles = append(vehicles, v)
		}
	}
	header := now
	if stamp := msg.GetHeader().GetTimestamp(); stamp != 0 {
		header = time.Unix(int64(stamp), 0)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	last := t.feeds[feed]
	next := snapshot{header: header, trains: map[string]*train{}, usesVehicles: last.usesVehicles || len(vehicles) > 0}
	if next.usesVehicles {
		for _, v := range vehicles {
			key := tripKey(v.GetTrip())
			if tr := t.trainFromVehicle(v, updates[key], last.trains[key], header, now); tr != nil {
				next.trains[key] = tr
			}
		}
	} else {
		for key, tu := range updates {
			if tr := t.trainFromUpdate(tu, last.trains[key], header, now); tr != nil {
				next.trains[key] = tr
			}
		}
	}
	t.feeds[feed] = next
}

func (t *Tracker) withRoute(trip *gtfs.TripDescriptor) *gtfs.TripDescriptor {
	if trip.GetRouteId() != "" {
		return trip
	}
	route := t.static.Route(trip.GetTripId())
	if route == "" {
		return trip
	}
	trip = proto.Clone(trip).(*gtfs.TripDescriptor)
	trip.RouteId = proto.String(route)
	return trip
}

func (t *Tracker) trainFromVehicle(v *gtfs.VehiclePosition, tu *gtfs.TripUpdate, prev *train, header, now time.Time) *train {
	timestamp := time.Unix(int64(v.GetTimestamp()), 0)
	if timestamp.After(header.Add(notStartedTolerance)) {
		return nil
	}
	obs, ok := observe(v, tu, t.static.HasStop)
	if !ok {
		return nil
	}
	trip := t.withRoute(v.GetTrip())
	vehicle := v.Vehicle
	if vehicle == nil {
		vehicle = tu.GetVehicle()
	}
	return &train{
		trip:      trip,
		vehicle:   vehicle,
		timestamp: timestamp,
		obs:       obs,
		leg:       t.nextLeg(prev, obs, trip.GetRouteId(), now),
	}
}

func (t *Tracker) trainFromUpdate(tu *gtfs.TripUpdate, prev *train, header, now time.Time) *train {
	switch tu.GetTrip().GetScheduleRelationship() {
	case gtfs.TripDescriptor_CANCELED, gtfs.TripDescriptor_DELETED:
		return nil
	}
	trip := t.withRoute(tu.GetTrip())
	visits, ok := scheduledVisits(tu, t.static, now)
	if !ok {
		visits = listedVisits(tu, t.static.HasStop)
	}
	obs, ok := observeVisits(visits, now)
	if !ok {
		return nil
	}
	if obs.at == "" && obs.from == "" {
		from, scheduled := t.static.PreviousStop(trip.GetTripId(), obs.to, obs.sequence)
		if scheduled && from == "" {
			return nil
		}
		obs.from = from
		// A trip not seen running before is taken to have set off once it
		// is due at its next stop within the time the hop takes.
		if prev == nil {
			if l := t.estimateLeg(obs, trip.GetRouteId()); l == nil || l.anchorAt.After(now) {
				return nil
			}
		}
	}
	timestamp := header
	if tu.Timestamp != nil {
		timestamp = time.Unix(int64(tu.GetTimestamp()), 0)
	}
	return &train{
		trip:      trip,
		vehicle:   tu.Vehicle,
		timestamp: timestamp,
		obs:       obs,
		leg:       t.nextLeg(prev, obs, trip.GetRouteId(), now),
	}
}

// nextLeg works out how to draw a train getting from wherever it is drawn
// now to where the feed says it is or is heading, so that it never jumps.
// A train reported standing at a stop it has not been drawn reaching is run
// there promptly.
func (t *Tracker) nextLeg(prev *train, obs observation, route string, now time.Time) *leg {
	standing := obs.at != ""
	target, eta := obs.to, obs.eta
	if standing {
		target, eta = obs.at, now
	}
	if prev == nil {
		if standing {
			return nil
		}
		return t.estimateLeg(obs, route)
	}

	origin := prev.obs.at
	if origin == "" {
		origin = prev.obs.to
	}
	var rest geo.Line
	var distance, speed float64
	unfinished := false
	if l := prev.leg; l != nil {
		origin = l.to
		distance, speed = l.advance(now)
		if distance < l.line.Length() {
			unfinished = true
			rest = l.line.Slice(distance, l.line.Length())
		}
	}

	if target == origin {
		switch {
		case prev.leg == nil, standing && !unfinished:
			return nil
		case standing && prev.obs.at == target:
			return prev.leg
		}
		l := *prev.leg
		l.anchor, l.anchorSpeed, l.anchorAt = distance, speed, now
		if standing {
			if l.eta.After(now) {
				l.eta = now
			}
		} else if !eta.IsZero() {
			l.eta = eta
		}
		return &l
	}

	segment := t.static.Segment(origin, target)
	// A segment with no run time is a straight line between stops that no
	// train runs between: the trip has changed course, so start afresh.
	if segment == nil || segment.RunTime == 0 || eta.IsZero() {
		if standing {
			return nil
		}
		return t.estimateLeg(obs, route)
	}
	l := &leg{to: target, line: segment.Line, eta: eta, ramp: t.config.Ramp, anchorAt: now}
	if unfinished {
		l.line = geo.Join(rest, segment.Line)
		l.anchorSpeed = speed
	}
	return l
}

// estimateLeg times a hop for a train whose departure was never seen, by
// assuming it will take the scheduled run time to reach the next stop.
func (t *Tracker) estimateLeg(obs observation, route string) *leg {
	from := obs.from
	if from == "" {
		from = t.static.Predecessor(obs.to, route)
	}
	segment := t.static.Segment(from, obs.to)
	if segment == nil || obs.eta.IsZero() {
		return nil
	}
	runTime := segment.RunTime
	if runTime == 0 {
		runTime = time.Duration(segment.Line.Length() / fallbackSpeed * float64(time.Second))
	}
	l := &leg{to: obs.to, line: segment.Line, eta: obs.eta, ramp: t.config.Ramp, anchorAt: obs.eta.Add(-runTime)}
	if !obs.departed.IsZero() {
		l.anchorAt = obs.departed
	}
	return l
}

type placement struct {
	point      geo.Point
	bearing    float64
	hasBearing bool
	status     gtfs.VehiclePosition_VehicleStopStatus
	stop       string
}

func (tr *train) place(now time.Time, data *static.Data) placement {
	if l := tr.leg; l != nil {
		distance, _ := l.advance(now)
		if tr.obs.at == "" || distance < l.line.Length() {
			status := gtfs.VehiclePosition_IN_TRANSIT_TO
			if tr.obs.incoming || tr.obs.at != "" {
				status = gtfs.VehiclePosition_INCOMING_AT
			}
			return placement{l.line.At(distance), l.line.BearingAt(distance), l.line.Length() > 0, status, l.to}
		}
	}
	status, stop := gtfs.VehiclePosition_STOPPED_AT, tr.obs.at
	if stop == "" {
		status, stop = gtfs.VehiclePosition_IN_TRANSIT_TO, tr.obs.to
	}
	bearing, hasBearing := data.Bearing(stop)
	return placement{data.Stops[stop], bearing, hasBearing, status, stop}
}

func (t *Tracker) Feed(now time.Time) *gtfs.FeedMessage {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var entities []*gtfs.FeedEntity
	for _, feed := range t.feeds {
		for key, tr := range feed.trains {
			if now.Sub(tr.timestamp) > t.config.MaxAge {
				continue
			}
			at := tr.place(now, t.static)
			position := &gtfs.Position{
				Latitude:  proto.Float32(float32(at.point.Lat)),
				Longitude: proto.Float32(float32(at.point.Lon)),
			}
			if at.hasBearing {
				position.Bearing = proto.Float32(float32(at.bearing))
			}
			var sequence *uint32
			if at.stop == tr.obs.at || at.stop == tr.obs.to {
				sequence = tr.obs.sequence
			}
			entities = append(entities, &gtfs.FeedEntity{
				Id: proto.String(key),
				Vehicle: &gtfs.VehiclePosition{
					Trip:                tr.trip,
					Vehicle:             tr.vehicle,
					Position:            position,
					CurrentStopSequence: sequence,
					StopId:              proto.String(at.stop),
					CurrentStatus:       at.status.Enum(),
					Timestamp:           proto.Uint64(uint64(tr.timestamp.Unix())),
				},
			})
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].GetId() < entities[j].GetId() })
	return &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{
			GtfsRealtimeVersion: proto.String("2.0"),
			Incrementality:      gtfs.FeedHeader_FULL_DATASET.Enum(),
			Timestamp:           proto.Uint64(uint64(now.Unix())),
		},
		Entity: entities,
	}
}

func tripKey(trip *gtfs.TripDescriptor) string {
	return trip.GetStartDate() + "_" + trip.GetTripId()
}
