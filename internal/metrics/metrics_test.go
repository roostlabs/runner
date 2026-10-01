package metrics

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestParseStatsMatchesTasksByIDPrefix(t *testing.T) {
	ids, tasks := parsePS("abcdef123456\tt-one\nfedcba654321\tt-two\n\n")
	if len(ids) != 2 || tasks["abcdef123456"] != "t-one" {
		t.Fatalf("ps = %v %v", ids, tasks)
	}

	got := parseStats("abcdef123456\t12.34%\t123.4MiB / 2GiB\nfedcba654321\t0.00%\t900kB / 2GiB\nunknown\t1%\t1MiB / 1GiB\n", tasks)
	if len(got) != 2 {
		t.Fatalf("stats = %+v", got)
	}
	if got[0].TaskID != "t-one" || !near(got[0].CPUPct, 12.34) || !near(got[0].MemMB, 123.4) {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].TaskID != "t-two" || got[1].CPUPct != 0 || !near(got[1].MemMB, 0.9) {
		t.Errorf("second = %+v", got[1])
	}

	// Stats prints short ids; ps printed long ones.
	short := parseStats("abcdef12\t5%\t1.5GiB / 2GiB\n", tasks)
	if len(short) != 1 || short[0].TaskID != "t-one" || !near(short[0].MemMB, 1536) {
		t.Errorf("short id = %+v", short)
	}
}

func TestParseBytesMB(t *testing.T) {
	cases := map[string]float64{
		"1.5GiB": 1536, "256MiB": 256, "512KiB": 0.5, "2GB": 2000, "7MB": 7, "500kB": 0.5, "0B": 0, "junk": 0,
	}
	for in, want := range cases {
		if got := parseBytesMB(in); !near(got, want) {
			t.Errorf("parseBytesMB(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDiskPercentIsAPercentage(t *testing.T) {
	got := diskPercent(t.TempDir())
	if got < 0 || got > 100 {
		t.Errorf("disk = %v", got)
	}
	if diskPercent("/definitely/not/here") != 0 {
		t.Error("a missing path did not read as zero")
	}
}

func TestHostFiguresStayInRange(t *testing.T) {
	s := &Sampler{DataDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		m := s.Host()
		for name, v := range map[string]float64{"cpu": m.CPU, "mem": m.Mem, "disk": m.Disk} {
			if v < 0 || v > 100 {
				t.Errorf("%s = %v", name, v)
			}
		}
		if m.Load < 0 {
			t.Errorf("load = %v", m.Load)
		}
	}
}

func TestClampPct(t *testing.T) {
	if clampPct(-1) != 0 || clampPct(101) != 100 || clampPct(50) != 50 {
		t.Error("clampPct")
	}
}
