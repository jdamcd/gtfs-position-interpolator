package statictest

import (
	"archive/zip"
	"bytes"
	"testing"
)

func Zip(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Network is a GTFS feed with four stops 1000 m apart, running north along
// one shape. Route 1 calls at all of them, scheduled at 120 s per hop.
// Route 2 skips 102N, taking 200 s from 101N to 103N. Route 1 also runs south
// from 102S to 101S. Stop 201N is 5 km east of the line and no trip serves it.
func Network() map[string]string {
	return map[string]string{
		"agency.txt": "agency_name,agency_url,agency_timezone\n" +
			"Test Transit,https://example.com,America/New_York\n",
		"stops.txt": "\ufeffstop_id,stop_name,stop_lat,stop_lon\n" +
			"101N,\"First, Av\",40.700000,-73.990000\n" +
			"102N,Second Av,40.708993,-73.990000\n" +
			"103N,Third Av,40.717986,-73.990000\n" +
			"104N,Fourth Av,40.726980,-73.990000\n" +
			"201N,Elsewhere,40.700000,-73.930700\n" +
			"101S,\"First, Av\",40.700000,-73.990000\n" +
			"102S,Second Av,40.708993,-73.990000\n",
		"routes.txt": "route_id,route_short_name,route_color,route_text_color\n" +
			"1,1,D82233,FFFFFF\n" +
			"2,2,F6BC26,000000\n",
		"trips.txt": "route_id,trip_id,service_id,shape_id\n" +
			"1,L1,Weekday,S1\n" +
			"1,L2,Weekday,S1\n" +
			"2,X1,Weekday,S1\n" +
			"1,L3,Weekday,S2\n",
		"shapes.txt": "shape_id,shape_pt_sequence,shape_pt_lat,shape_pt_lon\n" +
			"S1,0,40.700000,-73.990000\n" +
			"S1,1,40.704497,-73.990000\n" +
			"S1,2,40.708993,-73.990000\n" +
			"S1,3,40.713490,-73.990000\n" +
			"S1,4,40.717986,-73.990000\n" +
			"S1,5,40.722483,-73.990000\n" +
			"S1,6,40.726980,-73.990000\n" +
			"S2,0,40.708993,-73.990000\n" +
			"S2,1,40.700000,-73.990000\n",
		"stop_times.txt": "trip_id,stop_id,arrival_time,departure_time,stop_sequence\n" +
			"L1,101N,08:00:00,08:00:00,1\n" +
			"L1,102N,08:02:00,08:02:30,2\n" +
			"L1,103N,08:04:30,08:04:30,3\n" +
			"L1,104N,08:06:30,08:06:30,4\n" +
			"L2,101N,24:10:00,24:10:00,1\n" +
			"L2,102N,24:12:00,24:12:00,2\n" +
			"L2,103N,24:14:00,24:14:00,3\n" +
			"L2,104N,24:16:00,24:16:00,4\n" +
			"X1,101N,08:00:00,08:00:00,1\n" +
			"X1,103N,08:03:20,08:03:20,2\n" +
			"X1,104N,08:05:20,08:05:20,3\n" +
			"L3,102S,09:00:00,09:00:00,1\n" +
			"L3,101S,09:02:00,09:02:00,2\n",
	}
}
