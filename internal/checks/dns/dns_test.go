package dns_test

import (
	"context"
	"net"
	"testing"
	"time"

	xdns "golang.org/x/net/dns/dnsmessage"

	"github.com/mehrnet/radar-node/internal/checks/dns"
	"github.com/mehrnet/radar-node/internal/probe"
)

type qkey struct {
	name  string
	qtype xdns.Type
}

// startFakeDNS runs a minimal in-process UDP server answering whatever
// questions are declared in answers (keyed by fully-qualified name +
// query type) -- avoids depending on real DNS infrastructure being
// reachable from the test sandbox, and covers every record type this
// checker supports, not just A. An unmatched question gets an empty
// answer section, same as a real authoritative server's NODATA.
func startFakeDNS(t *testing.T, answers map[qkey][]xdns.ResourceBody) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var req xdns.Message
			if err := req.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := xdns.Message{
				Header:    xdns.Header{ID: req.Header.ID, Response: true, Authoritative: true},
				Questions: req.Questions,
			}
			if len(req.Questions) == 1 {
				q := req.Questions[0]
				for _, body := range answers[qkey{q.Name.String(), q.Type}] {
					resp.Answers = append(resp.Answers, xdns.Resource{
						Header: xdns.ResourceHeader{Name: q.Name, Type: q.Type, Class: xdns.ClassINET, TTL: 60},
						Body:   body,
					})
				}
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(packed, addr)
		}
	}()

	return conn.LocalAddr().String()
}

func mustName(t *testing.T, s string) xdns.Name {
	t.Helper()
	n, err := xdns.NewName(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func check(t *testing.T, server, target, record string) probe.Result {
	t.Helper()
	c := dns.New()
	return c.Check(context.Background(), probe.Options{
		Target:  target,
		Timeout: 2 * time.Second,
		Seq:     1,
		Params:  map[string]any{"server": server, "record": record},
	})
}

func TestCheck_ResolvesAgainstCustomServer(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"example.radar.test.", xdns.TypeA}: {&xdns.AResource{A: [4]byte{203, 0, 113, 42}}},
	})

	res := check(t, server, "example.radar.test.", "a")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 1 || answers[0] != "203.0.113.42" {
		t.Fatalf("expected [203.0.113.42], got %v", answers)
	}
}

// The real point of this one: two A records (cloudflare.com's own
// real-world shape) both come back in a single check, not just the
// first.
func TestCheck_MultipleARecordsAllReported(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"multi.radar.test.", xdns.TypeA}: {
			&xdns.AResource{A: [4]byte{104, 16, 133, 229}},
			&xdns.AResource{A: [4]byte{104, 16, 132, 229}},
		},
	})

	res := check(t, server, "multi.radar.test.", "a")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 2 {
		t.Fatalf("expected 2 answers, got %v", answers)
	}
}

func TestCheck_NoAnswerIsFailureForARecord(t *testing.T) {
	server := startFakeDNS(t, nil)
	res := check(t, server, "nowhere.radar.test.", "a")
	if res.Ok {
		t.Fatal("expected failure when no A records are returned")
	}
}

func TestCheck_NSRecords(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"example.radar.test.", xdns.TypeNS}: {
			&xdns.NSResource{NS: mustName(t, "ns1.radar.test.")},
			&xdns.NSResource{NS: mustName(t, "ns2.radar.test.")},
		},
	})

	res := check(t, server, "example.radar.test.", "ns")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 2 || answers[0] != "ns1.radar.test" || answers[1] != "ns2.radar.test" {
		t.Fatalf("expected [ns1.radar.test ns2.radar.test], got %v", answers)
	}
}

func TestCheck_NoNSRecordsIsFailure(t *testing.T) {
	server := startFakeDNS(t, nil)
	res := check(t, server, "nowhere.radar.test.", "ns")
	if res.Ok {
		t.Fatal("expected failure when no NS records are returned -- a domain with none can't actually resolve")
	}
}

func TestCheck_MXRecordsReportPreferencesAlongsideAnswers(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"example.radar.test.", xdns.TypeMX}: {
			&xdns.MXResource{Pref: 10, MX: mustName(t, "mail1.radar.test.")},
			&xdns.MXResource{Pref: 20, MX: mustName(t, "mail2.radar.test.")},
		},
	})

	res := check(t, server, "example.radar.test.", "mx")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 2 || answers[0] != "mail1.radar.test" || answers[1] != "mail2.radar.test" {
		t.Fatalf("expected [mail1.radar.test mail2.radar.test], got %v", answers)
	}
	preferences, _ := res.Extra["preferences"].([]int)
	if len(preferences) != 2 || preferences[0] != 10 || preferences[1] != 20 {
		t.Fatalf("expected [10 20], got %v", preferences)
	}
}

// Not every domain accepts mail -- zero MX records is a legitimate,
// common state, unlike zero A or NS records, and must not fail the
// check.
func TestCheck_NoMXRecordsIsOk(t *testing.T) {
	server := startFakeDNS(t, nil)
	res := check(t, server, "no-mail.radar.test.", "mx")
	if !res.Ok {
		t.Fatalf("expected ok with zero MX records, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 0 {
		t.Fatalf("expected no answers, got %v", answers)
	}
}

func TestCheck_TXTRecords(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"example.radar.test.", xdns.TypeTXT}: {
			&xdns.TXTResource{TXT: []string{"v=spf1 include:_spf.radar.test ~all"}},
		},
	})

	res := check(t, server, "example.radar.test.", "txt")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 1 || answers[0] != "v=spf1 include:_spf.radar.test ~all" {
		t.Fatalf("expected the SPF record, got %v", answers)
	}
}

func TestCheck_CNAMERecord(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"alias.radar.test.", xdns.TypeCNAME}: {
			&xdns.CNAMEResource{CNAME: mustName(t, "target.radar.test.")},
		},
	})

	res := check(t, server, "alias.radar.test.", "cname")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 1 || answers[0] != "target.radar.test" {
		t.Fatalf("expected [target.radar.test], got %v", answers)
	}
}

func TestCheck_PTRRecord(t *testing.T) {
	server := startFakeDNS(t, map[qkey][]xdns.ResourceBody{
		{"42.113.0.203.in-addr.arpa.", xdns.TypePTR}: {
			&xdns.PTRResource{PTR: mustName(t, "host.radar.test.")},
		},
	})

	res := check(t, server, "203.0.113.42", "ptr")
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 1 || answers[0] != "host.radar.test" {
		t.Fatalf("expected [host.radar.test], got %v", answers)
	}
}

func TestCheck_PTRRejectsANonIPTarget(t *testing.T) {
	res := check(t, "", "not-an-ip.radar.test", "ptr")
	if res.Ok {
		t.Fatal("expected failure for a non-IP PTR target")
	}
}

func TestCheck_NoPTRRecordIsOk(t *testing.T) {
	server := startFakeDNS(t, nil)
	res := check(t, server, "203.0.113.99", "ptr")
	if !res.Ok {
		t.Fatalf("expected ok with no reverse DNS entry, got error %q", res.Error)
	}
}

func TestCheck_UnsupportedRecordTypeFails(t *testing.T) {
	res := check(t, "", "example.radar.test", "soa")
	if res.Ok {
		t.Fatal("expected failure for an unsupported record type")
	}
}
