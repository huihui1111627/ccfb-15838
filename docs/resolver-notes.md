# Resolver behavior: verified facts and sources

This project's client-side resolver (`internal/resolv`) models the search-list
algorithm. The rules below were verified on this machine against the system
libresolv (macOS) with a recording UDP nameserver; the same behavior is
documented in `resolv.conf(5)`, glibc `resolv/res_query.c`
(`__res_context_search`), and Go's `net/dnsclient_unix.go`
(`dnsConfig.nameList`, `goLookupIPCNAMEOrder`).

Network access was unavailable while this was written, so the glibc source
could not be fetched; instead the system libc was used as the oracle. The
conformance tests in `internal/oracle` compile a small C harness that calls
`res_nsearch` with a fully overridden resolver state and compare its exact
query sequence with `internal/resolv`.

## Search order (verified)

With search list `svc.cluster.local cluster.local corp.internal`, `ndots=5`:

| query | queries sent |
|---|---|
| `api.stripe.com` | `.svc.cluster.local`, `.cluster.local`, `.corp.internal`, `.` |
| `redis` | `.svc.cluster.local`, `.cluster.local`, `.corp.internal`, `.` |
| `api.stripe.com.` (rooted) | as-is only |
| `api.stripe.com`, ndots=2 | as-is first, then suffixes |
| `example.com`, ndots=2 | suffixes first, then as-is |

So any unqualified external name costs `len(search)+1` candidate names, and a
dual-stack lookup costs twice that. This is the amplification the simulator
measures.

## Per-candidate failure handling (verified per response code)

Running libresolv with retry=1 against a server that always returns one code:

| server answer | libresolv behavior |
|---|---|
| NXDOMAIN | continue to the next search candidate |
| NODATA (NOERROR, no A) | continue to the next search candidate |
| SERVFAIL | retry per `retry`/`retrans`, then continue with next candidate |
| no answer (drop) | wait `retrans`, retry per `retry`, then stop the whole search with `h_errno=TRY_AGAIN` (1s elapsed with retrans=1 retry=1, 3.4s with retry=2) |
| A record | stop, success |

One correction worth recording: an early hand-written probe returned a
malformed NODATA packet and made it look as if libc stopped the search on
NODATA. The compiled conformance harness, which serves well-formed packets for
every rcode, showed that NODATA continues the search exactly like NXDOMAIN.
The simulator follows the harness, not the hand probe.

The simulator deliberately keeps the "drop stops the search" behavior, since
that is the multi-second tail it studies. It applies the same rules to the Go
policy except that the Go resolver moves on after a timeout.

## Other rules modeled

- At most 6 search domains (`MAXDNSRCH`), at most 3 nameservers (`MAXNS`).
- A suffixed candidate longer than 254 bytes is skipped.
- `ndots` counts dots in the unrooted input; default 1. Kubernetes pods
  default to `ndots:5`.
- `options single-request` serializes A and AAAA; the default issues both in
  parallel (two queries per candidate).
- `options attempts:N timeout:T` apply per candidate name, per nameserver.

## Intentional difference

The simulator counts a *lookup* that ends in a timeout as failed immediately
after the configured retry rounds instead of iterating the remaining search
candidates. libresolv on a dropped packet gives up the search (measured), so
the two agree for the libc policy; for the Go policy the simulator also stops,
because continuing after a 5s timeout would multiply the tail without adding
diagnostic value.

## Live-zone bridge

`internal/livezone` serves the simulated cluster zone over real UDP. The
`internal/livezone` test resolves a service short name and an external name
with the real libresolv through the same search list, then replays the same
lookup with `internal/resolv` against the same live server and requires the
candidate sequences to match.

## Why the Go policy exists in the model

`-client go` models the Go standard library's resolver (`PreferGo`, used when
a binary is built and run with `GODEBUG=netdns=go` or when the hostname is a
name the cgo resolver cannot handle). It differs from libc in two ways that
matter for this workload:

1. A and AAAA are always issued as a pair per candidate before the next
   candidate is tried (`goLookupIPCNAMEOrder`), so a search-list walk costs
   2 queries per candidate even when the first family misses.
2. The search loop only stops on a candidate that produced addresses; timeouts
   also move on to the next candidate, which can make one logical lookup take
   several per-query timeouts. The simulator caps that at the application
   deadline.

The default profile uses `glibc`, matching the C-based services in the fleet.
