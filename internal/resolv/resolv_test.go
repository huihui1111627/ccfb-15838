package resolv

import "testing"

// These cases pin the search-list expansion against the observed behavior of
// the system libresolv (see the conformance test in internal/oracle).
func TestNameList(t *testing.T) {
	k8s := Config{Ndots: 5, Search: []string{"prod.svc.cluster.local.", "svc.cluster.local.", "cluster.local.", "corp.internal."}}
	cases := []struct {
		name string
		cfg  Config
		in   string
		want []string
	}{
		{
			name: "k8s external name ndots5",
			cfg:  k8s,
			in:   "api.stripe.com",
			want: []string{
				"api.stripe.com.prod.svc.cluster.local.",
				"api.stripe.com.svc.cluster.local.",
				"api.stripe.com.cluster.local.",
				"api.stripe.com.corp.internal.",
				"api.stripe.com.",
			},
		},
		{
			name: "k8s in-cluster short name",
			cfg:  k8s,
			in:   "redis",
			want: []string{
				"redis.prod.svc.cluster.local.",
				"redis.svc.cluster.local.",
				"redis.cluster.local.",
				"redis.corp.internal.",
				"redis.",
			},
		},
		{
			name: "in-cluster two-label service name",
			cfg:  k8s,
			in:   "redis.prod",
			want: []string{
				"redis.prod.prod.svc.cluster.local.",
				"redis.prod.svc.cluster.local.",
				"redis.prod.cluster.local.",
				"redis.prod.corp.internal.",
				"redis.prod.",
			},
		},
		{
			name: "rooted external name skips search",
			cfg:  k8s,
			in:   "api.stripe.com.",
			want: []string{"api.stripe.com."},
		},
		{
			name: "ndots2 external tries as-is first",
			cfg:  Config{Ndots: 2, Search: []string{"svc.cluster.local.", "cluster.local."}},
			in:   "api.stripe.com",
			want: []string{"api.stripe.com.", "api.stripe.com.svc.cluster.local.", "api.stripe.com.cluster.local."},
		},
		{
			name: "ndots2 one-dot name tries search first",
			cfg:  Config{Ndots: 2, Search: []string{"svc.cluster.local.", "cluster.local."}},
			in:   "example.com",
			want: []string{"example.com.svc.cluster.local.", "example.com.cluster.local.", "example.com."},
		},
		{
			name: "ndots0 always as-is first",
			cfg:  Config{Ndots: 0, Search: []string{"cluster.local."}},
			in:   "redis",
			want: []string{"redis.", "redis.cluster.local."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.NameList(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("name %d: got %q want %q (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestNameListSkipsOverlongSuffixedNames(t *testing.T) {
	// 246 characters + ".cluster.local." exceeds 254.
	long := ""
	for len(long) < 240 {
		long += "a."
	}
	cfg := Config{Ndots: 5, Search: []string{"cluster.local."}}
	got := cfg.NameList(long)
	for _, n := range got {
		if len(n) > 254 {
			t.Fatalf("name %q length %d exceeds 254", n, len(n))
		}
	}
}
