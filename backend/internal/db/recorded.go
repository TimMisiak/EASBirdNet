package db

import (
	"path"
	"regexp"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo
)

// recorderZone is where the recorders are, and so what a time in a file name
// means when the name carries no offset.
var recorderZone = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// namedStart matches the start of a recording in its file name, as recorders
// write it: "Marymoor_20260723_160624(-0700).wav" began at 16:06:24 on July 23,
// local time, at UTC-7. The trailing boundary is what keeps a longer run of
// digits from matching. frontend/js/card-scan.js reads the same timestamp to
// date a file on the check page.
var namedStart = regexp.MustCompile(`(?:^|\D)(\d{8}_\d{6})(?:\D|$)`)

// namedOffset matches the UTC offset that may follow that time, in
// parentheses. It is matched on its own rather than as a second group of
// namedStart: RE2 has no lookahead, so namedStart's trailing boundary consumes
// the "(" that opens the offset, and one pattern for both silently never
// matches the offset of the very format it documents.
var namedOffset = regexp.MustCompile(`\(([+-]\d{4})\)`)

// RecordedAt is when a recording started, from its file name. The UTC offset
// in the name is used when there is one; otherwise the time is Pacific,
// which is where the recorders are.
func RecordedAt(cardPath string) (time.Time, bool) {
	name := path.Base(cardPath)
	m := namedStart.FindStringSubmatchIndex(name)
	if m == nil {
		return time.Time{}, false
	}
	stamp := name[m[2]:m[3]]
	loc := recorderZone
	// Search from the end of the time itself, not the end of the match, which
	// may have taken the offset's opening parenthesis with it.
	if o := namedOffset.FindStringSubmatch(name[m[3]:]); o != nil {
		offset, err := time.Parse("-0700", o[1])
		if err != nil {
			return time.Time{}, false
		}
		_, secs := offset.Zone()
		loc = time.FixedZone(o[1], secs)
	}
	t, err := time.ParseInLocation("20060102_150405", stamp, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
