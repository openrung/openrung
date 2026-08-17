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

Each file has a `version`. Editing an existing file means bumping it — row
additions included — and the Go suites pin the version they were written
against in a constant (loaded through `contract.LoadVersioned`, which owns the
version check), so a quiet edit fails the build on this side while the mobile
repo's check script catches the un-re-vendored copy on the other. The point of
the friction is that a deployed client's behavior cannot be renegotiated after
the fact: a token that changes meaning silently re-labels history in the
dashboard, and a relay field that stops decoding silently narrows which relays
a user can reach.

The bump itself is machine-enforced: the `contract-vectors-version` job in
`.github/workflows/go-checks.yml` fails any change to a vectors file whose
top-level `version` is not strictly greater than the previous one — a
downgrade or a reused number would make one version name two different
contents across history. A rename counts as an edit of the old file, and
deleting a vector file is refused outright until the mobile repo's vendoring
check exists to own the fallout. The job runs on pull requests and again on
every push to `main`: the repo has no up-to-date-branch protection, so two
independently green PRs can race the same bump and merge cleanly — the push
run judges exactly what landed and turns `main` red instead of shipping a
version that names content no PR tested.

The version is a coordination device between copies, so the rule starts when
the file first has a life outside its own pull request: within the PR that
introduces a file — before it has merged or been vendored anywhere — rows are
still being drafted and are edited in place, at the version they will land on,
and the CI job passes a file that is new on the branch. From merge onward every
edit needs a bump, with no exemptions, because once the mobile repo vendors a
file there is always a copy to coordinate.
