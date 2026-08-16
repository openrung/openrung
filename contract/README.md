# Cross-repo contract vectors

`vectors/` holds golden JSON vectors for behavior that clients in two repos have
to agree on, byte for byte, because the broker and the dashboard sit downstream
of all of them:

| File | Contract | Suites | Checked against |
| --- | --- | --- | --- |
| `classification.json` | The closed `failure_reason` token set and the ladder that produces it | go, kotlin, swift | `internal/clienttelemetry/classify.go`, `FailureClassifier.kt`, `FailureClassifier.swift` |
| `relay_decode.json` | Decoding a signed relay directory, and the usability filter over it | go, kotlin, swift, ts | `brokerapi`'s wire validation, `internal/relay`, `internal/client`'s selector, `src/model/relay.ts` |
| `broker_fronts.json` | The advertised broker fronts and the order discovery races them in | go, kotlin, swift, ts | `brokerapi.DefaultBrokerURLs`, `BrokerCandidates`, `EndpointUnboundBrokerFront`, and each client's hardcoded front list |

Each file carries its own doc header naming its consumers and describing the
shape of its rows. Read that header before editing one.

## Suites

Four suites consume this directory, but **not every file has every suite**, and
the `suites` field at the top of each file is what says which — the table above
just repeats it. `classification.json` has no `ts` suite because the React
Native layer never classifies a connect failure; the native tunnel providers do,
and there is no TypeScript classifier to run those rows against. Declaring a
suite that no row is claimed by is worse than omitting it: that suite iterates
the file, asserts nothing, and passes green forever, which reads as coverage.
`TestClassificationVectorsSuiteDeclaration` fails on exactly that.

- **Go** (this repo): `contract/relay_decode_test.go`,
  `contract/broker_fronts_test.go`, and
  `internal/clienttelemetry/contract_vectors_test.go`. They load the files
  through the `contract` package, which embeds them.
- **Kotlin, Swift, and Jest** (`openrung-mobile-app`): that repo vendors a copy
  of these files and its check script compares the vendored copy against a
  pinned `openrung` ref, so a drifted copy fails there rather than passing
  quietly against stale expectations.

Suites also consume a file at different depths, which the file's header spells
out. For `broker_fronts.json` the Go suite checks the whole snapshot because
`brokerapi` owns the list and the racing, while the mobile suites — which hand
native `brokerapi` a single primary and own no racing — check only that the
fronts they hardcode still appear in `default_order`.

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
