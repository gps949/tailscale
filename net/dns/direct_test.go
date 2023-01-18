// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	qt "github.com/frankban/quicktest"
	"tailscale.com/util/dnsname"
)

func TestDirectManager(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	testDirect(t, directFS{prefix: tmp})
}

type boundResolvConfFS struct {
	directFS
}

func (fs boundResolvConfFS) Rename(old, new string) error {
	if old == "/etc/resolv.conf" || new == "/etc/resolv.conf" {
		return errors.New("cannot move to/from /etc/resolv.conf")
	}
	return fs.directFS.Rename(old, new)
}

func (fs boundResolvConfFS) Remove(name string) error {
	if name == "/etc/resolv.conf" {
		return errors.New("cannot remove /etc/resolv.conf")
	}
	return fs.directFS.Remove(name)
}

func TestDirectBrokenRename(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	testDirect(t, boundResolvConfFS{directFS{prefix: tmp}})
}

func testDirect(t *testing.T, fs wholeFileFS) {
	const orig = "nameserver 9.9.9.9 # orig"
	resolvPath := "/etc/resolv.conf"
	backupPath := "/etc/resolv.pre-tailscale-backup.conf"

	if err := fs.WriteFile(resolvPath, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	readFile := func(t *testing.T, path string) string {
		t.Helper()
		b, err := fs.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	assertBaseState := func(t *testing.T) {
		if got := readFile(t, resolvPath); got != orig {
			t.Fatalf("resolv.conf:\n%s, want:\n%s", got, orig)
		}
		if _, err := fs.Stat(backupPath); !os.IsNotExist(err) {
			t.Fatalf("resolv.conf backup: want it to be gone but: %v", err)
		}
	}

	m := directManager{logf: t.Logf, fs: fs}
	if err := m.SetDNS(OSConfig{
		Nameservers:   []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")},
		SearchDomains: []dnsname.FQDN{"ts.net.", "ts-dns.test."},
		MatchDomains:  []dnsname.FQDN{"ignored."},
	}); err != nil {
		t.Fatal(err)
	}
	want := `# resolv.conf(5) file generated by tailscale
# For more info, see https://tailscale.com/s/resolvconf-overwrite
# DO NOT EDIT THIS FILE BY HAND -- CHANGES WILL BE OVERWRITTEN

nameserver 8.8.8.8
nameserver 8.8.4.4
search ts.net ts-dns.test
`
	if got := readFile(t, resolvPath); got != want {
		t.Fatalf("resolv.conf:\n%s, want:\n%s", got, want)
	}
	if got := readFile(t, backupPath); got != orig {
		t.Fatalf("resolv.conf backup:\n%s, want:\n%s", got, orig)
	}

	// Test that a nil OSConfig cleans up resolv.conf.
	if err := m.SetDNS(OSConfig{}); err != nil {
		t.Fatal(err)
	}
	assertBaseState(t)

	// Test that Close cleans up resolv.conf.
	if err := m.SetDNS(OSConfig{Nameservers: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	assertBaseState(t)
}

type brokenRemoveFS struct {
	directFS
}

func (b brokenRemoveFS) Rename(old, new string) error {
	return errors.New("nyaaah I'm a silly container!")
}

func (b brokenRemoveFS) Remove(name string) error {
	if strings.Contains(name, "/etc/resolv.conf") {
		return fmt.Errorf("Faking remove failure: %q", &fs.PathError{Err: syscall.EBUSY})
	}
	return b.directFS.Remove(name)
}

func TestDirectBrokenRemove(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	testDirect(t, brokenRemoveFS{directFS{prefix: tmp}})
}

func TestReadResolve(t *testing.T) {
	c := qt.New(t)
	tests := []struct {
		in      string
		want    OSConfig
		wantErr bool
	}{
		{in: `nameserver 192.168.0.100`,
			want: OSConfig{
				Nameservers: []netip.Addr{
					netip.MustParseAddr("192.168.0.100"),
				},
			},
		},
		{in: `nameserver 192.168.0.100 # comment`,
			want: OSConfig{
				Nameservers: []netip.Addr{
					netip.MustParseAddr("192.168.0.100"),
				},
			},
		},
		{in: `nameserver 192.168.0.100#`,
			want: OSConfig{
				Nameservers: []netip.Addr{
					netip.MustParseAddr("192.168.0.100"),
				},
			},
		},
		{in: `nameserver #192.168.0.100`, wantErr: true},
		{in: `nameserver`, wantErr: true},
		{in: `# nameserver 192.168.0.100`, want: OSConfig{}},
		{in: `nameserver192.168.0.100`, wantErr: true},

		{in: `search tailsacle.com`,
			want: OSConfig{
				SearchDomains: []dnsname.FQDN{"tailsacle.com."},
			},
		},
		{in: `search tailsacle.com # typo`,
			want: OSConfig{
				SearchDomains: []dnsname.FQDN{"tailsacle.com."},
			},
		},
		{in: `searchtailsacle.com`, wantErr: true},
		{in: `search`, wantErr: true},
	}

	for _, test := range tests {
		cfg, err := readResolv(strings.NewReader(test.in))
		if test.wantErr {
			c.Assert(err, qt.IsNotNil)
		} else {
			c.Assert(err, qt.IsNil)
		}
		c.Assert(cfg, qt.DeepEquals, test.want)
	}
}
