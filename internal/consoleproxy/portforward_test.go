// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestPortForwardTarget(t *testing.T) {
	user := &fluxvm.Record{}
	user.Request.Network.Mode = "user"
	user.Request.Network.Forwards = []fluxvm.RecordForward{{HostPort: 2222, GuestPort: 22}, {HostPort: 5353, GuestPort: 53, Protocol: "udp"}}

	tap := &fluxvm.Record{GuestIP: "10.0.0.5"}
	tap.Request.Network.Mode = "tap"

	tapNoIP := &fluxvm.Record{}
	tapNoIP.Request.Network.Mode = "tap"

	netns := &fluxvm.Record{GuestIP: "10.0.0.5"}
	netns.Request.Network.Mode = "tap"
	netns.Request.Network.NetNS = true

	cases := []struct {
		name     string
		rec      *fluxvm.Record
		port     uint16
		agentIPs []string
		hint     string
		want     string
		wantErr  bool
	}{
		{"user forwarded", user, 22, nil, "", "127.0.0.1:2222", false},
		{"user udp only", user, 53, nil, "", "", true},
		{"user not forwarded", user, 80, nil, "", "", true},
		{"tap record ip", tap, 22, nil, "", "10.0.0.5:22", false},
		{"tap hint not in candidates is ignored", tap, 22, nil, "192.0.2.1", "10.0.0.5:22", false},
		{"tap agent ip", tapNoIP, 22, []string{"10.0.0.7", "10.0.0.8"}, "10.0.0.8", "10.0.0.8:22", false},
		{"tap no address", tapNoIP, 22, nil, "", "", true},
		{"netns", netns, 22, nil, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := portForwardTarget(tc.rec, tc.port, tc.agentIPs, tc.hint)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
