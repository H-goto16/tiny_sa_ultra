// tinysa-logger: tinySA Ultra を常時掃引してCSVに記録する単一バイナリ。
//
//	tinysa-logger.exe log    [-port COM5] [-bands ...] [-interval 60] [-out logs]
//	tinysa-logger.exe report [-out logs] [-day 9-17] [-night 22-5]
//	tinysa-logger.exe ports
package main

import (
	"encoding/binary"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

var logFile *os.File

func say(a ...any) {
	line := fmt.Sprintln(a...)
	os.Stdout.WriteString(line)
	if logFile != nil {
		logFile.WriteString(line)
	}
}

func sayf(f string, a ...any) { say(strings.TrimRight(fmt.Sprintf(f, a...), "\n")) }

func waitEnter() {
	fmt.Print("\nEnter キーで閉じる...")
	var b [1]byte
	os.Stdin.Read(b[:])
}

type band struct {
	start, stop float64
	points      int
}

func parseFreq(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mul := 1.0
	switch {
	case strings.HasSuffix(s, "g"):
		mul, s = 1e9, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mul, s = 1e6, s[:len(s)-1]
	case strings.HasSuffix(s, "k"):
		mul, s = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	return v * mul, err
}

// "100k:30M:450,30M:300M" -> bands
func parseBands(s string, defPoints int) ([]band, error) {
	var out []band
	for _, item := range strings.Split(s, ",") {
		p := strings.Split(strings.TrimSpace(item), ":")
		if len(p) < 2 || len(p) > 3 {
			return nil, fmt.Errorf("bad band %q (start:stop[:points])", item)
		}
		a, err := parseFreq(p[0])
		if err != nil {
			return nil, err
		}
		b, err := parseFreq(p[1])
		if err != nil {
			return nil, err
		}
		n := defPoints
		if len(p) == 3 {
			if n, err = strconv.Atoi(p[2]); err != nil {
				return nil, err
			}
		}
		out = append(out, band{a, b, n})
	}
	return out, nil
}

// 候補ポートを優先度順に返す: VID:PID一致 > 名前に tinysa/STM/Virtual COM > その他USB > 全ポート。
func candidatePorts() []string {
	var hi, mid, lo []string
	seen := map[string]bool{}
	add := func(l *[]string, n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			*l = append(*l, n)
		}
	}
	if ports, err := enumerator.GetDetailedPortsList(); err == nil {
		for _, p := range ports {
			prod := strings.ToLower(p.Product + " " + p.Name)
			switch {
			case strings.EqualFold(p.VID, "0483") && strings.EqualFold(p.PID, "5740"):
				add(&hi, p.Name)
			case strings.Contains(prod, "tinysa") || strings.Contains(prod, "stmicro") || strings.Contains(prod, "virtual com"):
				add(&mid, p.Name)
			case p.IsUSB:
				add(&lo, p.Name)
			}
		}
	}
	// 列挙が失敗/不完全でもOSのポート一覧で補完
	if names, err := serial.GetPortsList(); err == nil {
		for _, n := range names {
			add(&lo, n)
		}
	}
	return append(append(hi, mid...), lo...)
}

// 実際に "version" を投げて tinySA か確認する。
func probe(name string) (*device, error) {
	dev, err := openDev(name)
	if err != nil {
		return nil, err
	}
	dev.p.SetReadTimeout(1500 * time.Millisecond)
	dev.p.SetDTR(true)
	dev.p.SetRTS(true)
	var resp []byte
	for try := 0; try < 3 && !strings.Contains(strings.ToLower(string(resp)), "tinysa"); try++ {
		dev.p.ResetInputBuffer()
		dev.p.Write([]byte("\r"))
		time.Sleep(100 * time.Millisecond)
		dev.p.Write([]byte("version\r"))
		resp = resp[:0]
		buf := make([]byte, 256)
		for len(resp) < 2000 {
			n, err := dev.p.Read(buf)
			if err != nil || n == 0 {
				break
			}
			resp = append(resp, buf[:n]...)
			if strings.Contains(string(resp), "ch> ") {
				break
			}
		}
	}
	if !strings.Contains(strings.ToLower(string(resp)), "tinysa") {
		dev.p.Close()
		return nil, fmt.Errorf("%s: tinySAの応答なし (%q)", name, strings.TrimSpace(string(resp)))
	}
	dev.p.SetReadTimeout(60 * time.Second)
	return dev, nil
}

// 指定ポート優先 -> 失敗なら自動検出にフォールバック。
func connect(explicit string) (*device, string, error) {
	var errs []string
	tried := map[string]bool{}
	var list []string
	if explicit != "" {
		list = append(list, explicit)
	}
	list = append(list, candidatePorts()...)
	for _, n := range list {
		if tried[n] {
			continue
		}
		tried[n] = true
		d, err := probe(n)
		if err == nil {
			return d, n, nil
		}
		errs = append(errs, err.Error())
	}
	if len(errs) == 0 {
		return nil, "", fmt.Errorf("シリアルポートが1つも見つからない (USB接続/ドライバ/ケーブル確認)")
	}
	return nil, "", fmt.Errorf("tinySAに接続できず: %s", strings.Join(errs, " | "))
}

type device struct{ p serial.Port }

func openDev(name string) (*device, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return nil, err
	}
	p.SetReadTimeout(60 * time.Second)
	return &device{p}, nil
}

func (d *device) readUntil(delim []byte) error {
	buf := make([]byte, 1)
	m := 0
	for m < len(delim) {
		n, err := d.p.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("timeout")
		}
		if buf[0] == delim[m] {
			m++
		} else if buf[0] == delim[0] {
			m = 1
		} else {
			m = 0
		}
	}
	return nil
}

func (d *device) readN(n int) ([]byte, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 4096)
	for len(out) < n {
		want := n - len(out)
		if want > len(buf) {
			want = len(buf)
		}
		k, err := d.p.Read(buf[:want])
		if err != nil {
			return nil, err
		}
		if k == 0 {
			return nil, fmt.Errorf("timeout")
		}
		out = append(out, buf[:k]...)
	}
	return out, nil
}

// scanraw 1回分。戻り値は dBm。
func (d *device) scan(b band) ([]float64, error) {
	d.p.ResetInputBuffer()
	cmd := fmt.Sprintf("scanraw %d %d %d\r", int64(b.start), int64(b.stop), b.points)
	if _, err := d.p.Write([]byte(cmd)); err != nil {
		return nil, err
	}
	if err := d.readUntil([]byte("{")); err != nil {
		return nil, err
	}
	body, err := d.readN(b.points * 3)
	if err != nil {
		return nil, err
	}
	if err := d.readUntil([]byte("ch> ")); err != nil {
		return nil, err
	}
	v := make([]float64, b.points)
	for i := range v {
		v[i] = float64(binary.LittleEndian.Uint16(body[i*3+1:]))/32 - 128
	}
	return v, nil
}

func freqs(bands []band) []float64 {
	var f []float64
	for _, b := range bands {
		for i := 0; i < b.points; i++ {
			f = append(f, b.start+(b.stop-b.start)*float64(i)/float64(b.points-1))
		}
	}
	return f
}

// 日別CSV。1行=interval秒間のmax-hold / 平均。ヘッダは周波数(Hz)。
type dayFile struct {
	day  string
	f    *os.File
	w    *csv.Writer
	kind string
}

func (d *dayFile) write(dir string, t time.Time, kind string, hdr []float64, vals []float64) error {
	day := t.Format("2006-01-02")
	if d.f == nil || d.day != day {
		if d.f != nil {
			d.w.Flush()
			d.f.Close()
		}
		path := filepath.Join(dir, fmt.Sprintf("%s_%s.csv", day, kind))
		_, statErr := os.Stat(path)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		d.f, d.w, d.day = f, csv.NewWriter(f), day
		if statErr != nil {
			row := []string{"time"}
			for _, h := range hdr {
				row = append(row, strconv.FormatFloat(h, 'f', 0, 64))
			}
			d.w.Write(row)
		}
	}
	row := []string{t.Format(time.RFC3339)}
	for _, v := range vals {
		row = append(row, strconv.FormatFloat(v, 'f', 1, 64))
	}
	d.w.Write(row)
	d.w.Flush()
	return d.w.Error()
}

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	port := fs.String("port", "", "COM port (省略で自動検出)")
	bandsS := fs.String("bands", "500k:30M,30M:300M,300M:1G,1G:3G,3G:6G", "start:stop[:points],...")
	pts := fs.Int("points", 450, "バンド毎の点数")
	interval := fs.Int("interval", 60, "記録間隔(秒)。この間のmax-holdと平均を1行に保存")
	out := fs.String("out", "logs", "出力ディレクトリ")
	fs.Parse(args)

	bands, err := parseBands(*bandsS, *pts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if f, err := os.OpenFile(filepath.Join(*out, "tinysa-logger.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		logFile = f
		defer f.Close()
	}
	hdr := freqs(bands)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	var maxF, avgF dayFile
	for {
		dev, name, err := connect(*port)
		if err != nil {
			say(time.Now().Format(time.RFC3339), err, "- retry in 10s")
			select {
			case <-stop:
				return nil
			case <-time.After(10 * time.Second):
			}
			continue
		}
		sayf("%s connected %s, %d points, interval %ds -> %s\n", time.Now().Format(time.RFC3339), name, len(hdr), *interval, *out)

		err = func() error {
			defer dev.p.Close()
			for {
				mx := make([]float64, len(hdr))
				sum := make([]float64, len(hdr))
				for i := range mx {
					mx[i] = math.Inf(-1)
				}
				cnt := 0
				deadline := time.Now().Add(time.Duration(*interval) * time.Second)
				for cnt == 0 || time.Now().Before(deadline) {
					select {
					case <-stop:
						return io.EOF
					default:
					}
					off := 0
					t0 := time.Now()
					var parts []string
					for _, b := range bands {
						v, err := dev.scan(b)
						if err != nil {
							return err
						}
						pk, pi := math.Inf(-1), 0
						for i, x := range v {
							if x > mx[off+i] {
								mx[off+i] = x
							}
							sum[off+i] += x
							if x > pk {
								pk, pi = x, i
							}
						}
						parts = append(parts, fmt.Sprintf("%s-%s:%.0fdBm@%s", fmtHz(b.start), fmtHz(b.stop), pk, fmtHz(hdr[off+pi])))
						off += b.points
					}
					cnt++
					sayf("%s sweep #%d (%.1fs) %s\n", time.Now().Format("15:04:05"), cnt, time.Since(t0).Seconds(), strings.Join(parts, " "))
				}
				for i := range sum {
					sum[i] /= float64(cnt)
				}
				now := time.Now()
				if err := maxF.write(*out, now, "max", hdr, mx); err != nil {
					return err
				}
				if err := avgF.write(*out, now, "avg", hdr, sum); err != nil {
					return err
				}
				sayf("%s >> saved row (%d sweeps, peak %.1f dBm) -> %s\n", now.Format("15:04:05"), cnt, maxOf(mx), *out)
			}
		}()
		if err == io.EOF {
			say("stopped")
			return nil
		}
		say(time.Now().Format(time.RFC3339), "error:", err, "- reconnecting in 5s")
		select {
		case <-stop:
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func fmtHz(f float64) string {
	switch {
	case f >= 1e9:
		return strconv.FormatFloat(f/1e9, 'f', -1, 64)[:min(5, len(strconv.FormatFloat(f/1e9, 'f', -1, 64)))] + "G"
	case f >= 1e6:
		return strconv.FormatFloat(math.Round(f/1e5)/10, 'f', -1, 64) + "M"
	}
	return strconv.FormatFloat(math.Round(f/1e3), 'f', -1, 64) + "k"
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

// "9-17" -> 時間帯判定(日跨ぎ可: "22-5")
func hourIn(spec string, h int) bool {
	p := strings.Split(spec, "-")
	a, _ := strconv.Atoi(p[0])
	b, _ := strconv.Atoi(p[1])
	if a <= b {
		return h >= a && h < b
	}
	return h >= a || h < b
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dir := fs.String("out", "logs", "ログディレクトリ")
	day := fs.String("day", "9-17", "つらい時間帯 (時-時)")
	night := fs.String("night", "22-5", "ましな時間帯 (時-時)")
	top := fs.Int("top", 25, "表示件数")
	kind := fs.String("kind", "avg", "avg|max")
	fs.Parse(args)

	files, _ := filepath.Glob(filepath.Join(*dir, "*_"+*kind+".csv"))
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no %s csv in %s", *kind, *dir)
	}
	var hdr []string
	var dsum, nsum []float64
	var dn, nn int
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			return err
		}
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		f.Close()
		if err != nil || len(rows) < 2 {
			continue
		}
		if hdr == nil {
			hdr = rows[0][1:]
			dsum, nsum = make([]float64, len(hdr)), make([]float64, len(hdr))
		}
		if len(rows[0])-1 != len(hdr) {
			continue
		}
		for _, row := range rows[1:] {
			if len(row) != len(hdr)+1 {
				continue
			}
			t, err := time.Parse(time.RFC3339, row[0])
			if err != nil {
				continue
			}
			h := t.Hour()
			var dst []float64
			switch {
			case hourIn(*day, h):
				dst = dsum
				dn++
			case hourIn(*night, h):
				dst = nsum
				nn++
			default:
				continue
			}
			for i, s := range row[1:] {
				v, _ := strconv.ParseFloat(s, 64)
				dst[i] += v
			}
		}
	}
	if dn == 0 || nn == 0 {
		return fmt.Errorf("not enough data: day rows=%d night rows=%d", dn, nn)
	}
	type res struct {
		f, d, n float64
	}
	var rs []res
	for i, h := range hdr {
		f, _ := strconv.ParseFloat(h, 64)
		rs = append(rs, res{f, dsum[i] / float64(dn), nsum[i] / float64(nn)})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].d-rs[i].n > rs[j].d-rs[j].n })
	fmt.Printf("day(%s) rows=%d  night(%s) rows=%d  kind=%s\n", *day, dn, *night, nn, *kind)
	fmt.Println("昼-夜 の差が大きい周波数:")
	fmt.Printf("%14s %9s %9s %8s\n", "freq(MHz)", "day dBm", "night dBm", "diff dB")
	for i := 0; i < *top && i < len(rs); i++ {
		r := rs[i]
		fmt.Printf("%14.3f %9.1f %9.1f %8.1f\n", r.f/1e6, r.d, r.n, r.d-r.n)
	}
	return nil
}

func main() {
	initConsole()
	args := os.Args[1:]
	interactive := len(args) == 0 // ダブルクリック起動
	if interactive {
		args = []string{"log"}
	}
	var err error
	switch args[0] {
	case "log":
		err = cmdLog(args[1:])
	case "report":
		err = cmdReport(args[1:])
	case "ports":
		var ps []*enumerator.PortDetails
		if ps, err = enumerator.GetDetailedPortsList(); err == nil {
			for _, p := range ps {
				fmt.Printf("%s usb=%v %s:%s %s\n", p.Name, p.IsUSB, p.VID, p.PID, p.Product)
			}
		}
	default:
		err = fmt.Errorf("unknown command %q (usage: log|report|ports)", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	if interactive || err != nil {
		waitEnter()
	}
	if err != nil {
		os.Exit(1)
	}
}
