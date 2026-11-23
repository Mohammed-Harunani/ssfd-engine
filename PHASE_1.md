# SSFD-Engine Phase 1
## Mathematical specification and canonical domain schema

Version 1.0 • Normative for the synthetic ledger. The Go package implements local type and shape validation and exact arithmetic. The stateful commit requirements below define the boundary that the ledger implementation must enforce.

### 1. Accounting perimeter and monetary representation

A book represents a single accounting entity. Account IDs, entry IDs and transaction IDs are unique within their respective types and books. Counterparty and rate IDs are unique within their types throughout a run. Every posting addresses exactly one account in its entry's book. Cross-institution settlement requires independently balanced entries in each institution's book with explicit settlement accounts. A book's trial balance can never be satisfied with an offset in another book.

A monetary value is a pair `(ISO 4217 alphabetic code, signed int64 minor units)`. A posting amount or transaction principal must be in [1, 2^63 − 1]. Debit and credit are explicit. A run pins a version and content digest of a currency catalog. The catalog admits only currencies with a defined supported minor-unit scale and fixes that scale for each admitted code across the run. A historical scale change requires separate datasets or an explicit redenomination design. A posting currency must match its account currency. A zero result in an audit remains associated with its currency. Use arbitrary-precision integer accumulators for sums, intermediate products, balances, and bounds checks. Any narrowing conversion must reject overflow before mutation.

### 2. Journal and trial balance

Let p be a posting of positive magnitude m(p), and define σ(p) as +1 for debit and −1 for credit. Every journal j contains two or more postings and satisfies the following condition for every currency c represented in j.

```text
D(j,c) = Σ m(p) over debit postings in currency c
C(j,c) = Σ m(p) over credit postings in currency c
D(j,c) − C(j,c) = 0
```

For account a, define its debit-positive balance B(a,k) as the sum of σ(p)m(p) over postings targeting a in committed entries through book commit sequence k. Genesis is empty and each opening balance is established through a balanced booking. For a book b and currency c, grouping B by account yields the trial balance invariant.

```text
Σ B(a,k) over accounts in (b,c) = 0
```

Induction proof. The empty book sums to zero. Each atomic commit changes the sum in each currency by the sum of its journal effects, which is zero by the entry invariant. A rejected request or successful identical retry contributes zero new postings. Therefore every committed prefix balances. The same result holds when selecting a subset of complete entries, including a single accounting period at a fixed knowledge cutoff. Splitting a journal across an audit boundary invalidates that argument.

Let asset and expense totals be debit-positive, and liability, equity and revenue totals be credit-positive. The partition sum is A + X − L − E − R = 0. Thus A = L + E + R − X per book and currency. E includes contributed capital and prior retained earnings. R and X are open income-statement balances. A period close moves net income to equity via balanced journals and resets the affected income-statement balances. This avoids counting the same income in both E and R − X. Debit or credit normality defines the reporting sign and does not impose a minimum balance. An authorized overdraft can cross zero under a separately configured limit.

A compound journal is an ordered group of incidences between a journal and its accounts. Its posting identity is (book, entry, one-based position). A graph projection can represent each debit as journal → account and each credit as account → journal. Repeated legs remain distinct. This preserves compound journals without manufacturing debit-to-credit pairings. Booked balances derive from postings. Authorization reserves affect spendable balances only.

### 3. Canonical records and identities

| Record | Identity | Required interpretation |
|---|---|---|
| Account | Book + account ID | One currency, one accounting class, purpose, opening instant, lifecycle state and synthetic owner. Owner zero is reserved for internal accounts. Class, purpose and currency are immutable. |
| Counterparty | Counterparty ID | Synthetic surrogate and one category (individual, business, institution). An account owner, debtor or creditor reference must resolve where required. |
| Transaction | Book + transaction ID | Source-scoped idempotency key, distinct financial intent and workflow state, principal, debtor and creditor, creation instant and optional FX quote. The same party can transfer between its accounts. |
| Journal entry | Book + entry ID | One associated transaction, entry kind, ordered postings, accounting-effective and recorded instants, and book commit sequence. Corrections reference an earlier committed entry. |
| Posting | Book + entry + one-based position | Scoped account, debit or credit side, and positive money. |
| FX rate | Rate ID | Ordered currency pair, positive rational rate and half-open validity interval. Immutable once registered. |
| Account or transaction event | Book + event sequence | An ordered lifecycle fact with previous and next states and recorded instant. Transaction events link to a journal when the transition commits one. |

The book sequence is a strictly increasing logical order for committed operations. Journal and event records from the same atomic operation may share its sequence across record types. One operation can produce at most one record of each kind for the same entity. Accounting-effective timestamps can be earlier than recording timestamps, subject to account opening, open-period and original-entry constraints. They cannot be later. Record times are nondecreasing per book. A commit validates business guards against current state even for backdated entries. Statements may order by (effective instant, commit sequence, entry ID) and must expose their knowledge cutoff when producing historical views. At ingestion normalize instants to UTC and remove monotonic metadata. Preserve sequence even if two recorded instants coincide. IDs are numeric synthetic surrogates. Canonical records accept no personal names, addresses, PANs or unbounded free text. Synthetic provenance also requires isolating input configuration and seeds from production records.

### 4. FX and reversals

A positive ratio n/d means quote major units per base major unit. For m source minor units and base and quote minor scales s_b and s_q, define:

```text
exact quote minor units = (m × n × 10^s_q) / (d × 10^s_b)
```

Compute quotient and remainder over integers. Increment the quotient when twice the remainder exceeds the denominator, or equals it and the quotient is odd. Reject a rounded target of zero or one outside the positive int64 range. Lock the exact rate ID and rounded quote at transaction creation after verifying the rate applies at that instant. Later clearing uses those fixed amounts. The fractional residual is the difference between the exact rational result and the rounded quote. Its magnitude is at most half a quote minor unit. Integer fees, revaluation, and realized gains or losses require their own balanced posting legs where modeled.

Example in a bank book. Exchange GBP 100.00 of a customer's debit-side reduction in deposit liability into USD 125.00 credited to a second deposit liability. Debit the GBP deposit 10,000 and credit the GBP FX position 10,000. Debit the USD FX position 12,500 and credit the USD deposit 12,500. Each currency independently balances. A translated valuation view is a separate derived calculation with its own rates and rounding policy.

A full reversal is a new journal with the original posting list in original order, preserving account, currency and magnitude and flipping each side. Use the original booked FX amounts. A full reversal references the original booking from the same transaction and can occur at most once. An adjustment, such as a partial refund, is a separate balanced journal with an explicit original reference. For principal refunds and chargebacks, the ordered account/currency/side totals against the original cannot exceed the corresponding remaining reversible totals. After any principal adjustment, a complete economic unwind uses an adjustment for the remainder and links it from the transition to Reversed. The remaining totals must then be zero. A zero remainder reached by the final partial operation uses that same linked adjustment. Entry validation checks the local reference shape. The commit layer checks original existence, order, accounting period, inverse equivalence and adjustment caps.

### 5. Lifecycle state machines

Accounts begin Active. Creation records an initial 0 → Active event. All ordinary state changes require an authorized simulated operation. Repeating an identical accepted request returns the original result.

| From | Allowed destinations | Guards |
|---|---|---|
| Active | Dormant, Frozen, Closed | Dormancy requires its configured inactivity threshold. Freeze imposes the configured restriction immediately. Closure requires zero booked balance and no open authorizations, holds or obligations. |
| Dormant | Active, Frozen, Closed | Reactivation requires a recorded reactivation action. Closure uses the same guard as Active. |
| Frozen | Active, Dormant | Release requires authorized clearance and an explicit destination supported by the account's inactivity history. |
| Closed | None | Terminal. |

Active permits ordinary activity subject to funds and holds. Dormant and Frozen block new customer-initiated outward activity. Dormant allows inbound credits and explicitly classified servicing entries. Frozen allows only operations explicitly permitted by its restriction, which defaults to blocking settlement. A correction is evaluated against the applicable classification and policy. Closed rejects all new postings. An accounting correction requiring movement after closure must use an eligible servicing account and a referenced adjustment.

Transactions begin Pending with a recorded 0 → Pending event.

| From | Allowed destinations | Required ledger effect |
|---|---|---|
| Pending | Authorized, Declined, Cancelled | Authorization locks the applicable reserve and validates account and funds guards. Decline and cancellation book no journal. |
| Authorized | Cleared, Cancelled | Clearing consumes the reserve and atomically appends one booking. Cancellation releases the reserve and books no journal. |
| Cleared | Disputed, Reversed | Dispute opening leaves the booking intact. Reversal requires an exact inverse or a terminal adjustment for all remaining principal. |
| Disputed | Cleared, Reversed | Returning to Cleared resolves the dispute without a second booking. Reversed requires a linked unwind. A partial adjustment can be booked while staying Disputed. |
| Reversed, Cancelled, Declined | None | Terminal. |

The journal link in a transaction event is required for Authorized → Cleared and all transitions to Reversed. It is otherwise zero. An adjustment that leaves workflow state unchanged is carried in journal history. An early clearing observation leaves the canonical state Pending until the prerequisite authorization is recorded. An authorization never by itself changes the trial balance. Holds are separate obligations enforced by the commit boundary so a held transaction can retain its payment state.

### 6. Commit and replay contract

1. Normalize and validate the complete request, including its stable identity and canonical business payload. Scope idempotency by book, source and request ID. Compare canonical payload bytes or a digest with collision-safe equality. Include amounts, references, expected transitions and ordered posting content. Exclude host-dependent metadata and generated commit sequence. A retry with identical content returns the original outcome. Reuse with changed content fails without ledger effects.
2. Resolve all foreign keys and validate entry-local rules. Validate as one operation against the latest committed state, including the original entry and adjustment limits, currency, book, open period, account lifecycle, available funds, holds, reserves, transaction transition and uniqueness.
3. Commit or reject atomically. A successful commit records the idempotency binding, sequence, journal and lifecycle effects together. Rejection leaves book state unchanged. An uncertain outcome is resolved by applying the idempotency rule under the same key. Concurrent commands must observe a serial order within a book. For reproducible generation the order comes from deterministic inputs and explicit tie-breakers.
4. Treat all committed records as immutable. Go slices and pointers are caller-owned before commit. The commit boundary must copy or transfer exclusive ownership and protect any returned snapshots from mutation. Pin the configuration, currency catalog, algorithm versions, input order and timestamp precision for replay. Canonical serialization must define byte ordering and sort unordered collections explicitly.

### 7. Verification and trade-offs

Run `go test -race -cover ./...` and `go vet ./...` from this directory with Go 1.22 or newer. The focused tests exercise overflow-resistant journal balancing, rejection of currency cancellation, scale conversion and ties-to-even rounding, rate expiry, and permitted and forbidden lifecycle transitions.

- Single-currency accounts make audit units explicit. An FX trade requires two balanced currency partitions and dedicated position accounts.
- Bounded money stays compact in individual records. Arbitrary-precision intermediates preserve correctness but incur allocation and computation costs to measure later.
- Ordered postings retain exact booking provenance. Immutable commit behavior depends on enforcing the ownership rules around Go records.
- Workflow separation preserves the distinction between reservations and settled accounting. Cross-record rules require the atomic ledger boundary.
- Declined and Cancelled make the state machine total for ordinary terminal failure paths. Partial disputes can remain open while adjustments accumulate.
- The currency catalog is supplied as a pinned reference input. Phase 1 includes GBP, USD and JPY as unit-test data.

### References

- OpenStax, [Expanded Accounting Equation](https://openstax.org/books/principles-financial-accounting/pages/3-2-define-and-describe-the-expanded-accounting-equation-and-its-relationship-to-analyzing-transactions).
- SIX, ISO 4217 Maintenance Agency, [Amendment 180](https://www.six-group.com/dam/download/financial-information/data-center/iso-currrency/amendments/dl-currency-iso-amendment-180.pdf), illustrating why currency catalog versions and minor-unit definitions are pinned.
