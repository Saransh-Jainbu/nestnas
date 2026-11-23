package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

// Health monitor: live transfer speeds, drive usage/temperature/health, CPU, memory and battery.
// Everything is read from the operating system (/proc and /sys on Linux) with no extra software.
// Detailed SMART data additionally needs the smartctl program and permission to use it; without
// it the monitor says so instead of guessing.

const monHistory = 120 // samples kept (one per second)

type monSample struct {
	T     int64   `json:"t"`
	In    float64 `json:"in"`  // bytes/s received (uploads to the NAS)
	Out   float64 `json:"out"` // bytes/s sent (downloads from the NAS)
	DiskR float64 `json:"dr"`  // drive read bytes/s
	DiskW float64 `json:"dw"`  // drive write bytes/s
	CPU   float64 `json:"cpu"` // percent busy
}

type monitor struct {
	in, out int64 // total bytes since start (atomic)

	mu      sync.Mutex
	hist    []monSample
	lastIn  int64
	lastOut int64
	lastAt  time.Time
	lastCPU [2]uint64 // busy, total
	lastDev string
	lastDS  diskStat
	peakIn  float64
	peakOut float64
	started time.Time

	smartAt  time.Time
	smartDev string
	smart    smartInfo
}

// ---------- counting what the server sends and receives ----------

type countReader struct {
	io.ReadCloser
	n *int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

type countWriter struct {
	http.ResponseWriter
	n *int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// ReadFrom keeps the fast file-copy path of the underlying connection.
func (c countWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		atomic.AddInt64(c.n, n)
		return n, err
	}
	return io.Copy(struct{ io.Writer }{c}, r)
}

func (c countWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------- drive and system readers ----------

type mountInfo struct{ src, dir, fs, opts string }

func readMountTable() []mountInfo {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []mountInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) >= 4 {
			out = append(out, mountInfo{unescapeMount(p[0]), unescapeMount(p[1]), p[2], p[3]})
		}
	}
	return out
}

// mountOf returns the mount that holds path (the longest matching mount point).
func mountOf(table []mountInfo, path string) (mountInfo, bool) {
	best, ok := mountInfo{}, false
	for _, m := range table {
		if path == m.dir || strings.HasPrefix(path, strings.TrimSuffix(m.dir, "/")+"/") {
			if !ok || len(m.dir) > len(best.dir) {
				best, ok = m, true
			}
		}
	}
	return best, ok
}

// wholeDisk maps a partition such as /dev/sda1 or /dev/nvme0n1p2 to its disk (sda, nvme0n1).
func wholeDisk(src string) string {
	if !strings.HasPrefix(src, "/dev/") {
		return ""
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	name := filepath.Base(src)
	p, err := filepath.EvalSymlinks("/sys/class/block/" + name)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(p, "partition")); err == nil {
		return filepath.Base(filepath.Dir(p))
	}
	return name
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(p string) (int64, bool) {
	v, err := strconv.ParseInt(readTrim(p), 10, 64)
	return v, err == nil
}

type diskStat struct{ rSect, wSect, ioMs uint64 }

func readDiskStat(dev string) (diskStat, bool) {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return diskStat{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 13 && f[2] == dev {
			r, _ := strconv.ParseUint(f[5], 10, 64)
			w, _ := strconv.ParseUint(f[9], 10, 64)
			io, _ := strconv.ParseUint(f[12], 10, 64)
			return diskStat{r, w, io}, true
		}
	}
	return diskStat{}, false
}

// readCPU returns busy and total CPU time from the first line of /proc/stat.
func readCPU() (busy, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	if len(f) < 6 || f[0] != "cpu" {
		return 0, 0, false
	}
	var v []uint64
	for _, x := range f[1:] {
		n, _ := strconv.ParseUint(x, 10, 64)
		v = append(v, n)
	}
	for _, n := range v {
		total += n
	}
	idle := v[3]
	if len(v) > 4 {
		idle += v[4] // iowait
	}
	return total - idle, total, true
}

func milliC(p string) (float64, bool) {
	v, ok := readInt(p)
	if !ok || v <= -1000 || v > 200000 {
		return 0, false
	}
	return float64(v) / 1000, true
}

func firstTemp(glob string) (float64, bool) {
	m, _ := filepath.Glob(glob)
	sort.Strings(m)
	for _, p := range m {
		if t, ok := milliC(p); ok {
			return t, true
		}
	}
	return 0, false
}

func cpuTemp() (float64, bool) {
	hw, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, h := range hw {
		switch readTrim(h + "/name") {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal":
			if t, ok := milliC(h + "/temp1_input"); ok {
				return t, true
			}
		}
	}
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		if t := readTrim(z + "/type"); strings.Contains(t, "x86_pkg_temp") || strings.Contains(t, "cpu") {
			if v, ok := milliC(z + "/temp"); ok {
				return v, true
			}
		}
	}
	return 0, false
}

func meminfo() (total, avail uint64) {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = n * 1024
			case "MemAvailable:":
				avail = n * 1024
			}
		}
	}
	return
}

func selfRSS() uint64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				n, _ := strconv.ParseUint(f[1], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

type batteryInfo struct {
	Present bool   `json:"present"`
	Percent int    `json:"percent"`
	Status  string `json:"status"`
	OnAC    bool   `json:"on_ac"`
}

func readBattery() batteryInfo {
	var b batteryInfo
	bats, _ := filepath.Glob("/sys/class/power_supply/BAT*")
	if len(bats) > 0 {
		b.Present = true
		if v, ok := readInt(bats[0] + "/capacity"); ok {
			b.Percent = int(v)
		}
		b.Status = readTrim(bats[0] + "/status")
	}
	supplies, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, s := range supplies {
		if readTrim(s+"/type") == "Mains" {
			if v, _ := readInt(s + "/online"); v == 1 {
				b.OnAC = true
			}
		}
	}
	return b
}

// ---------- SMART (needs smartctl) ----------

type smartInfo struct {
	Available    bool   `json:"available"`
	Reason       string `json:"reason,omitempty"`
	Passed       *bool  `json:"passed,omitempty"`
	TempC        *int   `json:"temp_c,omitempty"`
	PowerOnHours *int64 `json:"power_on_hours,omitempty"`
	Reallocated  *int64 `json:"reallocated,omitempty"`
	Pending      *int64 `json:"pending,omitempty"`
	Uncorrect    *int64 `json:"uncorrectable,omitempty"`
	PercentUsed  *int64 `json:"percent_used,omitempty"`
	MediaErrors  *int64 `json:"media_errors,omitempty"`
	CritWarning  *int64 `json:"critical_warning,omitempty"`
}

// parseSmart reads the JSON that "smartctl -j" prints.
func parseSmart(out []byte) smartInfo {
	var d struct {
		SmartStatus *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature *struct {
			Current int `json:"current"`
		} `json:"temperature"`
		PowerOn *struct {
			Hours int64 `json:"hours"`
		} `json:"power_on_time"`
		Ata *struct {
			Table []struct {
				ID  int `json:"id"`
				Raw struct {
					Value int64 `json:"value"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		Nvme *struct {
			CritWarning int64 `json:"critical_warning"`
			PercentUsed int64 `json:"percentage_used"`
			MediaErrors int64 `json:"media_errors"`
			Temperature int   `json:"temperature"`
			PowerOn     int64 `json:"power_on_hours"`
		} `json:"nvme_smart_health_information_log"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	if json.Unmarshal(out, &d) != nil {
		return smartInfo{Reason: "smartctl gave an unreadable answer"}
	}
	if d.SmartStatus == nil && d.Ata == nil && d.Nvme == nil {
		why := "this drive does not report SMART data"
		if len(d.Smartctl.Messages) > 0 {
			why = d.Smartctl.Messages[0].String
		}
		return smartInfo{Reason: why}
	}
	s := smartInfo{Available: true}
	if d.SmartStatus != nil {
		p := d.SmartStatus.Passed
		s.Passed = &p
	}
	if d.Temperature != nil {
		t := d.Temperature.Current
		s.TempC = &t
	}
	if d.PowerOn != nil {
		h := d.PowerOn.Hours
		s.PowerOnHours = &h
	}
	if d.Ata != nil {
		for _, a := range d.Ata.Table {
			v := a.Raw.Value
			switch a.ID {
			case 5:
				s.Reallocated = &v
			case 197:
				s.Pending = &v
			case 198:
				s.Uncorrect = &v
			}
		}
	}
	if d.Nvme != nil {
		c, p, m := d.Nvme.CritWarning, d.Nvme.PercentUsed, d.Nvme.MediaErrors
		s.CritWarning, s.PercentUsed, s.MediaErrors = &c, &p, &m
		if s.TempC == nil && d.Nvme.Temperature > 0 {
			t := d.Nvme.Temperature
			if t > 200 { // some versions report kelvin
				t -= 273
			}
			s.TempC = &t
		}
		if s.PowerOnHours == nil {
			h := d.Nvme.PowerOn
			s.PowerOnHours = &h
		}
	}
	return s
}

func runSmartctl(dev string) smartInfo {
	path, err := exec.LookPath("smartctl")
	if err != nil {
		return smartInfo{Reason: "install smartmontools (smartctl) for detailed drive health"}
	}
	var last smartInfo
	for _, mode := range []string{"auto", "sat"} { // many USB drives need "-d sat"
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		cmd := exec.CommandContext(ctx, path, "-j", "-H", "-A", "-i", "-d", mode, "/dev/"+dev)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		_ = cmd.Run() // smartctl uses non-zero exit codes to flag findings; the JSON is what matters
		cancel()
		last = parseSmart(buf.Bytes())
		if last.Available {
			return last
		}
		if strings.Contains(strings.ToLower(last.Reason), "permission") {
			last.Reason = "smartctl needs permission to read the drive (run NestNAS with access to the drive, or allow smartctl through sudo)"
			return last
		}
	}
	return last
}

// ---------- the report ----------

type alert struct {
	Level string `json:"level"` // "warn" or "crit"
	Text  string `json:"text"`
}

type driveReport struct {
	Path      string     `json:"path"`
	Device    string     `json:"device,omitempty"`
	Model     string     `json:"model,omitempty"`
	Kind      string     `json:"kind,omitempty"` // SSD, HDD, NVMe SSD
	USB       bool       `json:"usb"`
	Removable bool       `json:"removable"`
	FS        string     `json:"fs,omitempty"`
	ReadOnly  bool       `json:"read_only"`
	Total     uint64     `json:"total"`
	Free      uint64     `json:"free"`
	TempC     *float64   `json:"temp_c,omitempty"`
	TempNote  string     `json:"temp_note,omitempty"`
	Smart     *smartInfo `json:"smart,omitempty"`
	ReadBps   float64    `json:"read_bps"`
	WriteBps  float64    `json:"write_bps"`
	BusyPct   float64    `json:"busy_pct"`
	Connected bool       `json:"connected"`
}

func (s *Server) driveInfo(now time.Time) driveReport {
	path := s.st.StoragePath()
	d := driveReport{Path: path, Connected: s.storageOK()}
	d.Free, _ = diskFree(path)
	d.Total, _ = diskTotal(path)
	m, ok := mountOf(readMountTable(), path)
	if !ok {
		return d
	}
	d.FS = m.fs
	for _, o := range strings.Split(m.opts, ",") {
		if o == "ro" {
			d.ReadOnly = true
		}
	}
	dev := wholeDisk(m.src)
	if dev == "" {
		return d
	}
	d.Device = dev
	sys := "/sys/block/" + dev
	d.Model = readTrim(sys + "/device/model")
	rot, _ := readInt(sys + "/queue/rotational")
	rem, _ := readInt(sys + "/removable")
	d.Removable = rem == 1 || isRemovablePath(m.dir)
	if p, err := filepath.EvalSymlinks(sys); err == nil {
		d.USB = strings.Contains(p, "/usb")
	}
	switch {
	case strings.HasPrefix(dev, "nvme"):
		d.Kind = "NVMe SSD"
	case rot == 1:
		d.Kind = "HDD"
	default:
		d.Kind = "SSD"
	}
	// temperature from the kernel, if the drive offers it
	if t, ok := firstTemp(sys + "/device/hwmon/hwmon*/temp1_input"); ok {
		d.TempC = &t
	} else if t, ok := firstTemp(sys + "/device/nvme*/hwmon*/temp1_input"); ok {
		d.TempC = &t
	}
	s.mon.mu.Lock()
	if s.mon.smartDev != dev || now.Sub(s.mon.smartAt) > time.Minute {
		s.mon.smartDev, s.mon.smartAt = dev, now
		s.mon.mu.Unlock()
		si := runSmartctl(dev)
		s.mon.mu.Lock()
		s.mon.smart = si
	}
	si := s.mon.smart
	s.mon.mu.Unlock()
	d.Smart = &si
	if d.TempC == nil && si.TempC != nil {
		t := float64(*si.TempC)
		d.TempC = &t
	}
	if d.TempC == nil {
		d.TempNote = "This drive does not report its temperature to the computer. Installing smartmontools may enable it."
	}
	return d
}

// healthAlerts turns the readings into plain-language warnings.
func healthAlerts(d driveReport, cpuT *float64, memPct float64, bat batteryInfo) []alert {
	out := []alert{}
	add := func(l, t string) { out = append(out, alert{l, t}) }
	if !d.Connected {
		add("crit", "The storage drive is not connected.")
	}
	if d.ReadOnly {
		add("crit", "The storage drive is mounted read-only, so nothing can be saved. This often means the drive had errors.")
	}
	if d.Total > 0 {
		freePct := 100 * float64(d.Free) / float64(d.Total)
		switch {
		case freePct < 5:
			add("crit", "The storage drive is almost full (under 5% free).")
		case freePct < 10:
			add("warn", "The storage drive is getting full (under 10% free).")
		}
	}
	if d.TempC != nil {
		warn, crit := 65.0, 75.0
		if d.Kind == "HDD" {
			warn, crit = 50, 60
		}
		switch {
		case *d.TempC >= crit:
			add("crit", "The storage drive is very hot. Improve airflow or let it cool down.")
		case *d.TempC >= warn:
			add("warn", "The storage drive is running hot.")
		}
	}
	if sm := d.Smart; sm != nil && sm.Available {
		if sm.Passed != nil && !*sm.Passed {
			add("crit", "The drive reports that it is failing its own health check. Back up your files now and replace the drive.")
		}
		if sm.CritWarning != nil && *sm.CritWarning != 0 {
			add("crit", "The drive reports a critical warning.")
		}
		if (sm.Pending != nil && *sm.Pending > 0) || (sm.Uncorrect != nil && *sm.Uncorrect > 0) {
			add("warn", "The drive has sectors it could not read. Make sure you have a second copy of your files.")
		} else if sm.Reallocated != nil && *sm.Reallocated > 0 {
			add("warn", "The drive has replaced some damaged sectors. Keep an eye on it.")
		}
		if sm.PercentUsed != nil && *sm.PercentUsed >= 90 {
			add("warn", "The SSD has used most of its rated lifetime.")
		}
	}
	if cpuT != nil {
		switch {
		case *cpuT >= 95:
			add("crit", "The processor is extremely hot.")
		case *cpuT >= 85:
			add("warn", "The processor is running hot.")
		}
	}
	if memPct >= 92 {
		add("warn", "The computer is almost out of memory.")
	}
	if bat.Present && !bat.OnAC && bat.Percent <= 20 {
		add("warn", "The laptop is running on a low battery. The NAS goes offline if it shuts down.")
	}
	return out
}

func (s *Server) sample() {
	now := time.Now()
	in, out := atomic.LoadInt64(&s.mon.in), atomic.LoadInt64(&s.mon.out)
	s.mon.mu.Lock()
	defer s.mon.mu.Unlock()
	sm := monSample{T: now.Unix()}
	if !s.mon.lastAt.IsZero() {
		dt := now.Sub(s.mon.lastAt).Seconds()
		if dt > 0 {
			sm.In, sm.Out = float64(in-s.mon.lastIn)/dt, float64(out-s.mon.lastOut)/dt
			if sm.In > s.mon.peakIn {
				s.mon.peakIn = sm.In
			}
			if sm.Out > s.mon.peakOut {
				s.mon.peakOut = sm.Out
			}
			if m, ok := mountOf(readMountTable(), s.st.StoragePath()); ok {
				if dev := wholeDisk(m.src); dev != "" {
					if st, ok := readDiskStat(dev); ok {
						if s.mon.lastDev == dev {
							sm.DiskR = float64(st.rSect-s.mon.lastDS.rSect) * 512 / dt
							sm.DiskW = float64(st.wSect-s.mon.lastDS.wSect) * 512 / dt
						}
						s.mon.lastDev, s.mon.lastDS = dev, st
					}
				}
			}
		}
	}
	if busy, total, ok := readCPU(); ok {
		if s.mon.lastCPU[1] != 0 && total > s.mon.lastCPU[1] {
			sm.CPU = 100 * float64(busy-s.mon.lastCPU[0]) / float64(total-s.mon.lastCPU[1])
		}
		s.mon.lastCPU = [2]uint64{busy, total}
	}
	s.mon.lastIn, s.mon.lastOut, s.mon.lastAt = in, out, now
	s.mon.hist = append(s.mon.hist, sm)
	if len(s.mon.hist) > monHistory {
		s.mon.hist = s.mon.hist[len(s.mon.hist)-monHistory:]
	}
}

func (s *Server) monitorLoop() {
	s.mon.started = time.Now()
	t := time.NewTicker(time.Second)
	for range t.C {
		s.sample()
	}
}

func avgLast(h []monSample, n int, f func(monSample) float64) float64 {
	if len(h) == 0 {
		return 0
	}
	if n > len(h) {
		n = len(h)
	}
	var sum float64
	for _, x := range h[len(h)-n:] {
		sum += f(x)
	}
	return sum / float64(n)
}

func (s *Server) monitorAPI(w http.ResponseWriter, r *http.Request, u config.User) {
	now := time.Now()
	drive := s.driveInfo(now)
	s.mon.mu.Lock()
	hist := append([]monSample(nil), s.mon.hist...)
	peakIn, peakOut, started := s.mon.peakIn, s.mon.peakOut, s.mon.started
	s.mon.mu.Unlock()
	if n := len(hist); n > 0 {
		drive.ReadBps = avgLast(hist, 3, func(x monSample) float64 { return x.DiskR })
		drive.WriteBps = avgLast(hist, 3, func(x monSample) float64 { return x.DiskW })
	}
	if dev := drive.Device; dev != "" {
		// busy% over the last couple of seconds, from the kernel's I/O time counter
		if st, ok := readDiskStat(dev); ok {
			s.mon.mu.Lock()
			if s.mon.lastDev == dev && !s.mon.lastAt.IsZero() {
				dt := now.Sub(s.mon.lastAt).Seconds() + 1
				drive.BusyPct = 100 * float64(st.ioMs-s.mon.lastDS.ioMs) / (dt * 1000)
				if drive.BusyPct < 0 {
					drive.BusyPct = 0
				} else if drive.BusyPct > 100 {
					drive.BusyPct = 100
				}
			}
			s.mon.mu.Unlock()
		}
	}
	total, avail := meminfo()
	var memPct float64
	if total > 0 {
		memPct = 100 * float64(total-avail) / float64(total)
	}
	var cpuT *float64
	if t, ok := cpuTemp(); ok {
		cpuT = &t
	}
	bat := readBattery()
	load := strings.Fields(readTrim("/proc/loadavg"))
	var up float64
	if f := strings.Fields(readTrim("/proc/uptime")); len(f) > 0 {
		up, _ = strconv.ParseFloat(f[0], 64)
	}
	alerts := healthAlerts(drive, cpuT, memPct, bat)
	status := "ok"
	for _, a := range alerts {
		if a.Level == "crit" {
			status = "crit"
		} else if status == "ok" {
			status = "warn"
		}
	}
	jsonOut(w, 200, map[string]interface{}{
		"status": status, "alerts": alerts,
		"transfer": map[string]interface{}{
			"in_bps": avgLast(hist, 3, func(x monSample) float64 { return x.In }), "out_bps": avgLast(hist, 3, func(x monSample) float64 { return x.Out }),
			"peak_in": peakIn, "peak_out": peakOut,
			"total_in": atomic.LoadInt64(&s.mon.in), "total_out": atomic.LoadInt64(&s.mon.out),
			"since": started.Unix(),
		},
		"drive":   drive,
		"history": hist,
		"system": map[string]interface{}{
			"os": runtime.GOOS, "cpus": runtime.NumCPU(),
			"cpu_pct":  avgLast(hist, 3, func(x monSample) float64 { return x.CPU }),
			"cpu_temp": cpuT, "mem_total": total, "mem_used": total - avail, "mem_pct": memPct,
			"load": load, "uptime": up, "nestnas_rss": selfRSS(), "battery": bat,
		},
	})
}
