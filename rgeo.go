/*
Copyright 2020 Sam Smith

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License.  You may obtain a copy of the
License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied.  See the License for the
specific language governing permissions and limitations under the License.
*/

/*
Package rgeo is a fast, simple solution for local reverse geocoding.

Rather than relying on external software or online APIs, rgeo packages all of
the data it needs in your binary. This means it will only works down to the
level of cities, but if that's all you need then this is the library for you.

rgeo uses data from https://naturalearthdata.com, if your coordinates are going
to be near specific borders I would advise checking the data beforehand (links
to which are in the files). If you want to use your own dataset, check out the
datagen folder.

# Installation

	go get github.com/sams96/rgeo

# Contributing

Contributions are welcome, I haven't got any guidelines or anything so maybe
just make an issue first.
*/
package rgeo

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/paulmach/orb"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/golang/geo/s2"
	"github.com/paulmach/orb/geojson"
)

// ErrLocationNotFound is returned when no country is found for given
// coordinates.
var ErrLocationNotFound = errors.New("country not found")

// Location is the return type for ReverseGeocode.
type Location struct {
	// Commonly used country name
	Country string `json:"country,omitempty"`

	// Formal name of country
	CountryLong string `json:"country_long,omitempty"`

	// ISO 3166-1 alpha-1 and alpha-2 codes
	CountryCode2 string `json:"country_code_2,omitempty"`
	CountryCode3 string `json:"country_code_3,omitempty"`

	Continent string `json:"continent,omitempty"`
	Region    string `json:"region,omitempty"`
	SubRegion string `json:"subregion,omitempty"`

	Province string `json:"province,omitempty"`

	// ISO 3166-2 code
	ProvinceCode string `json:"province_code,omitempty"`

	County string `json:"county,omitempty"`

	City string `json:"city,omitempty"`
}

type LocationWithGeometry struct {
	Location
	Geometry orb.Geometry `json:"geometry"`
}

type GeomLookup map[string]map[s2.Shape]orb.Geometry

// getFunctionName returns rgeo.Countries110, rgeo.Countries10, etc.
func getFunctionName(i interface{}) string {
	return filepath.Base(runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name())
}

// Rgeo is the type used to hold pre-created polygons for reverse geocoding.
type Rgeo struct {
	index *s2.ShapeIndex
	locs  map[s2.Shape]Location
	geoms GeomLookup
	query *s2.ContainsPointQuery
}

// Go generate commands to regenerate the included datasets, this assumes you
// have the GeoJSON files from
// https://github.com/nvkelso/natural-earth-vector/tree/master/geojson.
// go run datagen/datagen.go -ne -o Countries110 ne_110m_admin_0_countries.geojson
// go run datagen/datagen.go -ne -o Countries10 ne_10m_admin_0_countries.geojson
// go run datagen/datagen.go -ne -o Provinces10 -merge ne_10m_admin_0_countries.geojson ne_10m_admin_1_states_provinces.geojson
// go run datagen/datagen.go -ne -o Cities10 ne_10m_urban_areas_landscan.geojson

// New returns an Rgeo struct which can then be used with ReverseGeocode. It
// takes any number of datasets as an argument. The included datasets are:
// Countries110, Countries10, Provinces10 and Cities10. Provinces10 includes all
// of the country information so if that's all you want don't use Countries as
// well. Cities10 only includes cities so you'll probably want to use
// Provinces10 with it.
func New(datasets ...func() []byte) (*Rgeo, error) {
	// Parse GeoJSON
	var fc geojson.FeatureCollection

	// Initialise Rgeo struct
	ret := new(Rgeo)
	ret.index = s2.NewShapeIndex()
	ret.locs = make(map[s2.Shape]Location)
	ret.geoms = GeomLookup{}

	for i, dataset := range datasets {
		br := bytes.NewReader(dataset())
		if br.Len() == 0 {
			return nil, fmt.Errorf("no data in dataset %d", i)
		}

		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("decompression failed for dataset %d: %w", i, err)
		}

		// Parse GeoJSON
		var tfc geojson.FeatureCollection
		if err := json.NewDecoder(zr).Decode(&tfc); err != nil {
			return nil, fmt.Errorf("invalid JSON in dataset %d: %w", i, err)
		}

		if err := zr.Close(); err != nil {
			return nil, fmt.Errorf("failed to close gzip reader for dataset %d: %w", i, err)
		}

		fc.Features = append(fc.Features, tfc.Features...)

		datasetName := getFunctionName(dataset)
		shpGeoms, ok := ret.geoms[datasetName]
		if !ok {
			shpGeoms = make(map[s2.Shape]orb.Geometry, len(tfc.Features))
			ret.geoms[datasetName] = shpGeoms
		}
		for _, c := range tfc.Features {
			// Convert GeoJSON features from geom (multi)polygons to s2 polygons
			p, err := polygonFromGeometry(c.Geometry)
			if err != nil {
				return nil, fmt.Errorf("bad polygon in geometry: %w", err)
			}
			ret.geoms[datasetName][p] = c.Geometry

			ret.index.Add(p)

			// The s2 ContainsPointQuery returns the shapes that contain the given
			// point, but I haven't found any way to attach the location information
			// to the shapes, so I use a map to get the information.
			loc := getLocationStrings(c.Properties)
			ret.locs[p] = loc
		}
	}

	ret.query = s2.NewContainsPointQuery(ret.index, s2.VertexModelOpen)

	return ret, nil
}

func (r *Rgeo) DatasetNames() []string {
	names := make([]string, 0, len(r.geoms))
	for k := range r.geoms {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.Compare(names[i], names[j]) < 0
	})
	return names
}

// ReverseGeocode returns the country in which the given coordinate is located.
//
// The input is a geom.Coord, which is just a []float64 with the longitude
// in the zeroth position and the latitude in the first position
// (i.e. []float64{lon, lat}).
func (r *Rgeo) ReverseGeocode(loc orb.Point) (Location, error) {
	res := r.query.ContainingShapes(pointFromCoord(loc))
	if len(res) == 0 {
		return Location{}, ErrLocationNotFound
	}

	return r.combineLocations(res), nil
}

// combineLocations combines the Locations for the given s2 Shapes.
func (r *Rgeo) combineLocations(s []s2.Shape) (l Location) {
	for _, shape := range s {
		loc := r.locs[shape]
		l = Location{
			Country:      firstNonEmpty(l.Country, loc.Country),
			CountryLong:  firstNonEmpty(l.CountryLong, loc.CountryLong),
			CountryCode2: firstNonEmpty(l.CountryCode2, loc.CountryCode2),
			CountryCode3: firstNonEmpty(l.CountryCode3, loc.CountryCode3),
			Continent:    firstNonEmpty(l.Continent, loc.Continent),
			Region:       firstNonEmpty(l.Region, loc.Region),
			SubRegion:    firstNonEmpty(l.SubRegion, loc.SubRegion),
			Province:     firstNonEmpty(l.Province, loc.Province),
			ProvinceCode: firstNonEmpty(l.ProvinceCode, loc.ProvinceCode),
			County:       firstNonEmpty(l.County, loc.County),
			City:         firstNonEmpty(l.City, loc.City),
		}
	}

	return
}

func (r *Rgeo) ReverseGeocodeWithGeometry(loc orb.Point, dataset string) (LocationWithGeometry, error) {
	res := r.query.ContainingShapes(pointFromCoord(loc))
	if len(res) == 0 {
		return LocationWithGeometry{}, ErrLocationNotFound
	}
	if dataset == "" {
		return LocationWithGeometry{}, fmt.Errorf("missing parameter: geometry dataset")
	}
	shpGeom, ok := r.geoms[dataset]
	if !ok {
		return LocationWithGeometry{}, fmt.Errorf("dataset not found: %q", dataset)
	}
	out := LocationWithGeometry{Location: r.combineLocations(res)}
	// Assign the geometry that has a match on shape in this dataset.
	for _, shp := range res {
		geom, ok := shpGeom[shp]
		if ok {
			out.Geometry = geom
			return out, nil
		}
	}
	return LocationWithGeometry{}, fmt.Errorf("no geometry found for dataset %q", dataset)
}

// firstNonEmpty returns the first non empty parameter.
func firstNonEmpty(s ...string) string {
	for _, i := range s {
		if i != "" {
			return i
		}
	}

	return ""
}

// Get the relevant strings from the GeoJSON properties.
func getLocationStrings(p map[string]interface{}) Location {
	loc := Location{
		Country:      getPropertyString(p, "ADMIN", "admin"),
		CountryLong:  getPropertyString(p, "FORMAL_EN"),
		CountryCode2: getPropertyString(p, "ISO_A2"),
		CountryCode3: getPropertyString(p, "ISO_A3"),
		Continent:    getPropertyString(p, "CONTINENT"),
		Region:       getPropertyString(p, "REGION_UN"),
		SubRegion:    getPropertyString(p, "SUBREGION"),
		Province:     getPropertyString(p, "name"),
		ProvinceCode: getPropertyString(p, "iso_3166_2"),
		City:         strings.TrimSuffix(getPropertyString(p, "name_conve"), "2"),
	}
	if t, ok := p["TYPE"]; ok && t == "County" {
		loc.County = getPropertyString(p, "NAME")
	}
	return loc
}

// getPropertyString gets the value from a map given the key as a string, or
// from the next given key if the previous fails.
func getPropertyString(m map[string]interface{}, keys ...string) (s string) {
	var ok bool
	for _, k := range keys {
		s, ok = m[k].(string)
		if ok {
			break
		}
	}

	return
}

// polygonFromGeometry converts a geom.T to an s2 Polygon.
func polygonFromGeometry(g orb.Geometry) (*s2.Polygon, error) {
	var (
		polygon *s2.Polygon
		err     error
	)

	switch t := g.(type) {
	case orb.Polygon:
		polygon, err = polygonFromPolygon(t)
	case orb.MultiPolygon:
		polygon, err = polygonFromMultiPolygon(t)
	default:
		return nil, errors.New("needs Polygon or MultiPolygon")
	}

	if err != nil {
		return nil, err
	}

	return polygon, nil
}

// Converts a geom MultiPolygon to an s2 Polygon.
func polygonFromMultiPolygon(p orb.MultiPolygon) (*s2.Polygon, error) {
	loops := make([]*s2.Loop, 0, len(p))

	for i := 0; i < len(p); i++ {
		this, err := loopSliceFromPolygon(p[i])
		if err != nil {
			return nil, err
		}

		loops = append(loops, this...)
	}

	return s2.PolygonFromLoops(loops), nil
}

// Converts a geom Polygon to an s2 Polygon.
func polygonFromPolygon(p orb.Polygon) (*s2.Polygon, error) {
	loops, err := loopSliceFromPolygon(p)
	return s2.PolygonFromLoops(loops), err
}

// Converts a geom Polygon to slice of s2 Loop.
//
// Modified from types.loopFromPolygon from github.com/dgraph-io/dgraph.
func loopSliceFromPolygon(p orb.Polygon) ([]*s2.Loop, error) {
	loops := make([]*s2.Loop, 0, len(p))

	for i := 0; i < len(p); i++ {
		r := p[i]
		n := len(r)

		if n < 4 {
			return nil, errors.New("can't convert ring with less than 4 points")
		}

		if !r[0].Equal(r[n-1]) {
			return nil, fmt.Errorf(
				"last coordinate not same as first for polygon: %+v", p)
		}

		// S2 specifies that the orientation of the polygons should be CCW.
		// However there is no restriction on the orientation in WKB (or
		// GeoJSON). To get the correct orientation we assume that the polygons
		// are always less than one hemisphere. If they are bigger, we flip the
		// orientation.
		reverse := isClockwise(r)
		l := loopFromRing(r, reverse)

		// Since our clockwise check was approximate, we check the cap and
		// reverse if needed.
		if l.CapBound().Radius().Degrees() > 90 {
			// Remaking the loop sometimes caused problems, this works better
			l.Invert()
		}

		loops = append(loops, l)
	}

	return loops, nil
}

// Checks if a ring is clockwise or counter-clockwise. Note: This uses the
// algorithm for planar polygons and doesn't work for spherical polygons that
// contain the poles or the antimeridan discontinuity. We use this as a fast
// approximation instead.
//
// From github.com/dgraph-io/dgraph
func isClockwise(r orb.Ring) bool {
	return r.Orientation() == -1
	// The algorithm is described here
	// https://en.wikipedia.org/wiki/Shoelace_formula
	//var a float64
	//
	//n := r.NumCoords()
	//
	//for i := 0; i < n; i++ {
	//	p1 := r.Coord(i)
	//	p2 := r.Coord((i + 1) % n)
	//	a += (p2.X() - p1.X()) * (p1.Y() + p2.Y())
	//}
	//
	//return a > 0
}

// From github.com/dgraph-io/dgraph
func loopFromRing(r orb.Ring, reverse bool) *s2.Loop {
	// In WKB, the last coordinate is repeated for a ring to form a closed loop.
	// For s2 the points aren't allowed to repeat and the loop is assumed to be
	// closed, so we skip the last point.
	n := len(r)
	pts := make([]s2.Point, n-1)

	for i := 0; i < n-1; i++ {
		var c orb.Point
		if reverse {
			c = r[n-1-i]
		} else {
			c = r[i]
		}

		pts[i] = pointFromCoord(c)
	}

	return s2.LoopFromPoints(pts)
}

// From github.com/dgraph-io/dgraph
func pointFromCoord(r orb.Point) s2.Point {
	// The GeoJSON spec says that coordinates are specified as [long, lat]
	// We assume that any data encoded in the database follows that format.
	ll := s2.LatLngFromDegrees(r.Y(), r.X())
	return s2.PointFromLatLng(ll)
}

// String method for type Location.
func (l Location) String() string {
	ret := "<Location>"

	// Special case for empty location
	if l == (Location{}) {
		return ret + " Empty Location"
	}

	// Add city name
	if l.City != "" {
		ret += " " + l.City + ","
	}

	// Add province name
	if l.Province != "" {
		ret += " " + l.Province + ","
	}

	// Add country name
	if l.Country != "" {
		ret += " " + l.Country
	} else if l.CountryLong != "" {
		ret += " " + l.CountryLong
	}

	// Add country code in brackets
	if l.CountryCode3 != "" {
		ret += " (" + l.CountryCode3 + ")"
	} else if l.CountryCode2 != "" {
		ret += " (" + l.CountryCode2 + ")"
	}

	// Add continent/region
	if len(ret) > len("<Location>") {
		ret += ","
	}

	switch {
	case l.Continent != "":
		ret += " " + l.Continent
	case l.Region != "":
		ret += " " + l.Region
	case l.SubRegion != "":
		ret += " " + l.SubRegion
	}

	return ret
}
