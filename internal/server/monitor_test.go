package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMountOfPicksLongestMountPoint(t *testing.T) {
	table := []mountInfo{
		{"/dev/nvme0n1p2", "/", "ext4", "rw"},
		{"/dev/sda1", "/media/me/One Touch", "exfat", "rw,nosuid"},
		{"/dev/sdb1", "/media/me/One", "ext4", "rw"},
	}
	m, ok := mountOf(table, "/media/me/One Touch/NAS")
	if !ok || m.src != "/dev/sda1" {
		t.Fatalf("got %+v", m)
	}
	if m, _ := mountOf(table, "/media/me/One Touchy"); m.src != "/dev/nvme0n1p2" {
		t.Fatalf("a similar name must not match: %+v", m)
	}
	if m, _ := mountOf(table, "/home/me/NestNAS-Storage"); m.src != "/dev/nvme0n1p2" {
		t.Fatalf("got %+v", m)
	}
}

func TestParseSmartATA(t *testing.T) {
	js := `{"smart_status":{"passed":true},"temperature":{"current":38},"power_on_time":{"hours":1200},
	"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}},{"id":197,"raw":{"value":0}},{"id":198,"raw":{"value":0}}]}}`
	s := parseSmart([]byte(js))
	if !s.Available || s.Passed == nil || !*s.Passed || *s.TempC != 38 || *s.PowerOnHours != 1200 || *s.Reallocated != 8 {
		t.Fatalf("%+v", s)
	}
}

func TestParseSmartNVMe(t *testing.T) {
	js := `{"smart_status":{"passed":true},"nvme_smart_health_information_log":{"critical_warning":0,"percentage_used":93,"media_errors":0,"temperature":41,"power_on_hours":50}}`
	s := parseSmart([]byte(js))
	if !s.Available || *s.PercentUsed != 93 || *s.TempC != 41 {
		t.Fatalf("%+v", s)
	}
}

func TestParseSmartUnavailable(t *testing.T) {
	s := parseSmart([]byte(`{"smartctl":{"messages":[{"string":"Unknown USB bridge"}]}}`))
	if s.Available || s.Reason != "Unknown USB bridge" {
		t.Fatalf("%+v", s)
	}
	if parseSmart([]byte("not json")).Available {
		t.Fatal("garbage must not look like data")
	}
}

func TestHealthAlerts(t *testing.T) {
	hot := 80.0
	passed := false
	d := driveReport{Connected: true, Total: 1000, Free: 70, Kind: "SSD", TempC: &hot, Smart: &smartInfo{Available: true, Passed: &passed}}
	got := healthAlerts(d, nil, 50, batteryInfo{})
	kinds := map[string]int{}
	for _, a := range got {
		kinds[a.Level]++
	}
	// almost full (warn), very hot (crit), failing SMART (crit)
	if kinds["crit"] != 2 || kinds["warn"] != 1 {
		t.Fatalf("%+v", got)
	}
	ok := healthAlerts(driveReport{Connected: true, Total: 1000, Free: 900, Kind: "SSD"}, nil, 40, batteryInfo{Present: true, Percent: 80, OnAC: true})
	if len(ok) != 0 {
		t.Fatalf("a healthy system must have no alerts: %+v", ok)
	}
	if a := healthAlerts(driveReport{Connected: false}, nil, 0, batteryInfo{}); len(a) != 1 || a[0].Level != "crit" {
		t.Fatalf("%+v", a)
	}
	hdd := 52.0
	if a := healthAlerts(driveReport{Connected: true, Kind: "HDD", TempC: &hdd}, nil, 0, batteryInfo{}); len(a) != 1 || a[0].Level != "warn" {
		t.Fatalf("HDDs run cooler: %+v", a)
	}
	if a := healthAlerts(driveReport{Connected: true}, nil, 0, batteryInfo{Present: true, Percent: 10}); len(a) != 1 {
		t.Fatalf("low battery on battery power: %+v", a)
	}
}

func TestCountingWrappers(t *testing.T) {
	var in, out int64
	r := countReader{io.NopCloser(strings.NewReader("hello world")), &in}
	if _, err := io.Copy(io.Discard, r); err != nil || atomic.LoadInt64(&in) != 11 {
		t.Fatalf("in=%d err=%v", in, err)
	}
	rec := httptest.NewRecorder()
	cw := countWriter{rec, &out}
	cw.Write([]byte("12345"))
	if _, err := cw.ReadFrom(strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if out != 8 || rec.Body.String() != "12345abc" {
		t.Fatalf("out=%d body=%q", out, rec.Body.String())
	}
}
