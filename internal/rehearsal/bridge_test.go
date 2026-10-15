// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"context"
	"strings"
	"testing"
)

const goodGuard = `{"nftables":[
 {"metainfo":{"version":"1.0"}},
 {"table":{"family":"inet","name":"krh_demo"}},
 {"chain":{"family":"inet","table":"krh_demo","name":"input","type":"filter","hook":"input","prio":-300,"policy":"accept"}},
 {"chain":{"family":"inet","table":"krh_demo","name":"output","type":"filter","hook":"output","prio":-300,"policy":"accept"}},
 {"rule":{"family":"inet","table":"krh_demo","chain":"input","expr":[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"krh-demo"}},{"drop":null}]}},
 {"rule":{"family":"inet","table":"krh_demo","chain":"output","expr":[{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"krh-demo"}},{"drop":null}]}}
]}`

func TestIsolationGuardFailsClosed(t *testing.T) {
	if err := validateGuard([]byte(goodGuard), "krh-demo", "krh_demo"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"accept instead of drop", `"drop":null`, `"accept":null`},
		{"wrong interface", `"right":"krh-demo"`, `"right":"production"`},
		{"wrong comparison", `"op":"=="`, `"op":"!="`},
		{"wrong hook", `"hook":"output"`, `"hook":"forward"`},
		{"wrong priority", `"prio":-300`, `"prio":0`},
		{"missing output chain", `"chain":{"family":"inet","table":"krh_demo","name":"output"`, `"other":{"family":"inet","table":"krh_demo","name":"output"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateGuard([]byte(strings.Replace(goodGuard, tc.from, tc.to, 1)), "krh-demo", "krh_demo"); err == nil {
				t.Fatal("unsafe guard accepted")
			}
		})
	}
	if err := validateGuard([]byte(`{}`), "krh-demo", "krh_demo"); err == nil {
		t.Fatal("empty guard accepted")
	}
	for _, name := range []string{"production", "../../etc", "krh-no-such"} {
		if err := CheckBridge(context.Background(), name); err == nil {
			t.Fatalf("accepted absent/invalid bridge %s", name)
		}
	}
}
