# Cross-repo contract vectors

`vectors/` holds golden JSON vectors for behavior that clients in two repos have
to agree on, byte for byte, because the broker and the dashboard sit downstream
of all of them:

| File | Contract | Checked against |
| --- | --- | --- |
| `classification.json` | The closed `failure_reason` token set and the ladder that produces it | `internal/clienttelemetry/classify.go`, `FailureClassifier.kt`, `FailureClassifier.swift` |
| `relay_decode.json` | Decoding a signed relay directory, and the usability filter over it | `brokerapi`'s wire validation, `internal/relay`, `internal/client`'s selector, `src/model/relay.ts` |
| `broker_fronts.json` | The advertised broker fronts and the order discovery races them in | `brokerapi.DefaultBrokerURLs`, `BrokerCandidates`, `EndpointUnboundBrokerFront` |

Each file carries its own doc header naming its consumers and describing the
shape of its rows. Read that header before editing one.

## Suites

Four suites consume these files:

- **Go** (this repo): `contract/relay_decode_test.go`,
  `contract/broker_fronts_test.go`, and
  `internal/clienttelemetry/contract_vectors_test.go`. They load the files
  through the `contract` package, which embeds them.
- **Kotlin, Swift, and Jest** (`openrung-mobile-app`): that repo vendors a copy
  of these files and its check script compares the vendored copy against a
  pinned `openrung` ref, so a drifted copy fails there rather than passing
  quietly against stale expectations.

A suite runs every row it claims. Where a row cannot exist on a platform — a
Winsock errno on Android, say — it says so in the row itself, with a `platform`
or `suites` field and a note explaining why, so a skip is a stated fact rather
than a silent gap.

## Changing a vector

Each file has a `version`. Editing an existing row means bumping it, and the Go
suites pin the version they were written against in a constant, so a quiet edit
fails the build on this side while the mobile repo's check script catches the
un-re-vendored copy on the other. The point of the friction is that a deployed
client's behavior cannot be renegotiated after the fact: a token that changes
meaning silently re-labels history in the dashboard, and a relay field that
stops decoding silently narrows which relays a user can reach.
