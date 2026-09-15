package main

import (
	"bytes"
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
