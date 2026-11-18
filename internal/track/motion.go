package track

// The fastest a vehicle is drawn moving, in metres per second.
const maxSpeed = 36.0

// advance moves a train along its segment. It starts at distance metres with
// the given speed and should reach length after remaining seconds, standing
// still. The speed changes linearly to a cruising speed over ramp seconds,
// holds, then falls linearly to zero over the last ramp seconds. A train
// whose arrival is closer than it could manage at maxSpeed arrives late
// instead. All distances are in metres and times in seconds.
func advance(distance, speed, length, remaining, ramp, elapsed float64) (float64, float64) {
	togo := length - distance
	if togo <= 0 {
		return length, 0
	}
	if elapsed <= 0 {
		return distance, speed
	}
	remaining = max(remaining, togo/maxSpeed+ramp)
	ramp = min(ramp, remaining/2)
	cruise := (togo - speed*ramp/2) / (remaining - ramp)

	if cruise < 0 {
		// Too close to slow down gently: brake evenly to stop at the end.
		stopIn := 2 * togo / speed
		if elapsed >= stopIn {
			return length, 0
		}
		v := speed * (1 - elapsed/stopIn)
		return distance + (speed+v)/2*elapsed, v
	}

	switch {
	case elapsed < ramp:
		v := speed + (cruise-speed)*elapsed/ramp
		return distance + (speed+v)/2*elapsed, v
	case elapsed < remaining-ramp:
		return distance + (speed+cruise)/2*ramp + cruise*(elapsed-ramp), cruise
	case elapsed < remaining:
		left := remaining - elapsed
		v := cruise * left / ramp
		return length - v*left/2, v
	}
	return length, 0
}
