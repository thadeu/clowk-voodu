package main

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWireAddLine(t *testing.T) {
	id := wireIdentity{PublicKey: "KEY=", Address: "10.254.91.221", ListenPort: 51820, Endpoint: "152.53.91.221:51820"}

	if got, want := wireAddLine(id), "vd wire add --key KEY= --address 10.254.91.221 --endpoint 152.53.91.221:51820"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	id.Endpoint = ""

	if got := wireAddLine(id); strings.Contains(got, "--endpoint") {
		t.Fatalf("no endpoint known, yet printed: %q", got)
	}
}

func TestRenderWireList(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	link := func(handshake string, rx, tx int64) *struct {
		Endpoint      string `json:"endpoint,omitempty"`
		LastHandshake string `json:"last_handshake,omitempty"`
		RxBytes       int64  `json:"rx_bytes"`
		TxBytes       int64  `json:"tx_bytes"`
	} {
		return &struct {
			Endpoint      string `json:"endpoint,omitempty"`
			LastHandshake string `json:"last_handshake,omitempty"`
			RxBytes       int64  `json:"rx_bytes"`
			TxBytes       int64  `json:"tx_bytes"`
		}{Endpoint: "152.53.167.105:51820", LastHandshake: handshake, RxBytes: rx, TxBytes: tx}
	}

	st := wireStatus{
		Identity: wireIdentity{PublicKey: "LOCALKEY12345", Address: "10.254.91.221"},
		Peers: []wirePeer{
			{PublicKey: "PEERKEY2xxxxxx", Address: "10.254.167.105", Applied: true, Link: link("2026-09-13T11:59:48Z", 2048, 100)},
			{PublicKey: "PEERKEY3xxxxxx", Address: "10.254.200.7", Applied: true, Link: link("", 0, 0)},
			{PublicKey: "PEERKEY4xxxxxx", Address: "10.254.5.5", Endpoint: "1.2.3.4:51820"},
		},
	}

	var out bytes.Buffer

	renderWireList(&out, st, now)

	got := out.String()

	for _, want := range []string{
		"this host: 10.254.91.221  key LOCALKEY…",
		"10.254.167.105  152.53.167.105:51820  12s ago      2.0KB/100B",
		"10.254.200.7    152.53.167.105:51820  never",
		"10.254.5.5      1.2.3.4:51820         not applied  -",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderWireList_Empty(t *testing.T) {
	var out bytes.Buffer

	renderWireList(&out, wireStatus{Identity: wireIdentity{Address: "10.254.91.221"}}, time.Now())

	if !strings.Contains(out.String(), "No peers") {
		t.Fatalf("got:\n%s", out.String())
	}
}

func ufwFixture() wireUFW {
	return wireUFW{Bridge: "br-21f70aa6d28e", Rules: [][]string{
		{"allow", "51820/udp"},
		{"route", "allow", "in", "on", "wg0", "out", "on", "br-21f70aa6d28e"},
	}}
}

func TestRenderUFWRules(t *testing.T) {
	got := renderUFWRules(ufwFixture(), "sudo ")
	want := "sudo ufw allow 51820/udp\nsudo ufw route allow in on wg0 out on br-21f70aa6d28e\n"

	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyUFW_WithoutRootPrintsAndRefuses(t *testing.T) {
	var out bytes.Buffer

	called := false
	err := applyUFW(&out, ufwFixture(), false, 1000, func(...string) (string, error) { called = true; return "", nil })

	if err == nil || !strings.Contains(err.Error(), "needs root") || called {
		t.Fatalf("err = %v, called = %v", err, called)
	}

	if !strings.Contains(out.String(), "sudo ufw allow 51820/udp") {
		t.Fatalf("rules not printed for the operator:\n%s", out.String())
	}
}

func TestApplyUFW_EnableAndDisableRunTheRightArgs(t *testing.T) {
	if _, err := exec.LookPath("ufw"); err != nil {
		t.Skip("ufw not on PATH; the root path is covered where it exists")
	}

	var ran []string

	run := func(args ...string) (string, error) {
		ran = append(ran, strings.Join(args, " "))

		return "Rule added", nil
	}

	if err := applyUFW(io.Discard, ufwFixture(), false, 0, run); err != nil {
		t.Fatal(err)
	}

	if err := applyUFW(io.Discard, ufwFixture(), true, 0, run); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"allow 51820/udp",
		"route allow in on wg0 out on br-21f70aa6d28e",
		"delete allow 51820/udp",
		"route delete allow in on wg0 out on br-21f70aa6d28e",
	}

	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %v, want %v", ran, want)
	}
}

func TestDeleteArgs(t *testing.T) {
	if got := strings.Join(deleteArgs([]string{"allow", "51820/udp"}), " "); got != "delete allow 51820/udp" {
		t.Fatal(got)
	}

	if got := strings.Join(deleteArgs([]string{"route", "allow", "in", "on", "wg0"}), " "); got != "route delete allow in on wg0" {
		t.Fatal(got)
	}
}
