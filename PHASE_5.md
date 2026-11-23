# SSFD-Engine Phase 5 protocol serialization

## Running the exports

```sh
go run ./cmd/ssfd-demo -seed 42 -start-year 2024 -years 1 -export-dir output-2024
go run ./cmd/ssfd-demo -seed 42 -chaos-profile aggressive -export-dir output-chaos
go test -race -cover ./...
go vet ./...
```

The CLI still prints its normal summary or, with `-snapshot`, full result to stdout. The original Phase 2 fixture remains available through `-phase2-fixture`. The CLI rejects combining that fixture with `-export-dir` because it lacks the behavioral channel data used for routing.

`serializers.ExportRun` builds its artifacts from an immutable snapshot. It validates journal balance and references, replays account balances and cross-checks the supplied trial balance. It writes into a new directory and installs the manifest last. Use the presence of a valid manifest and matching file hashes to identify a complete bundle. Files are assembled in memory before publication. Consumers use profile and manifest metadata for content verification. Programmatic clients can provide an explicit ISO 4217 catalog of codes, numeric codes and minor-unit scales. The bundled simulation uses GBP 826 and USD 840 with two digits each.

## Representation and reconciliation

| File | Content |
|---|---|
| `canonical.json` | Complete typed agent result, ordered journal and posting history, recorded and effective times, state and hold history, FX references and amounts, and optional chaos metadata. This is the lossless reconstruction source. JSON readers should preserve unsigned integer precision when parsing IDs. |
| `pain.001.001.09.xml` | One booked credit-transfer instruction per mapped payroll, P2P, internal savings transfer or cross-border payment. Omitted when a supplied run contains no eligible transfer. |
| `camt.053.001.08.xml` | A statement per ledger account, including internal GL accounts as synthetic statement subjects. Contains every booked posting and inception opening and closing booked balances. |
| `card-ascii.dat`, `card-binary.dat` | Both encodings of the same card lifecycle selection. Use the index to resolve identities and the DE7 year. |
| `ledger-ascii.dat`, `ledger-cp037.dat`, `ledger-cp500.dat` | Each is a complete lossless projection of the canonical posting stream with exact signs, record counts and totals. |
| `manifest.json` | Export version, book and sequence, currency definitions, gross posting totals, ending per-currency trial balance and SHA-256 of each data file. |

Individual wire formats have distinct scopes and field limits. A credit initiation has no representation for the bank's entire booked journal or lifecycle. Card authorization and clearing are lifecycle messages for one amount. Their values must be reconciled by MTI and transaction rather than summed as two charges. The manifest distinguishes gross posting totals from the trial balance of net account balances. No currency values are summed across currency codes.

## ISO 20022 profiles

The exporter uses the official v09 credit-transfer initiation and v08 statement namespaces. XML amounts are formatted from integer minor units without floating point. The formatter enforces ISO 20022 amount precision limits for these versions and fails on unrepresentable values.

- `pain.001.001.09` groups one selected transaction per `PmtInf`. Source and beneficiary accounts and amounts are bound to canonical bookings. References use journal and transaction IDs, and the requested execution instant is the accounting-effective instant. Synthetic account IDs use the Other identification branch. FX uses the native `EqvtAmt` choice to carry the source and target currency, plus one namespaced supplementary quote carrying the exact booked target and rate ID. Bank- and scheme-specific implementation rules may require extensions to this research profile. Generated initiation messages are retrospective synthetic fixtures.
- `camt.053.001.08` emits one `Ntry` per posting in recorded order, with both booking and value time. It shows credit when the posting increases the account's normal-side balance. Thus a credit increases a deposit balance and a debit increases an asset balance. References and supplementary text preserve canonical side, journal, sequence, transaction and original-entry linkage. Full reversal postings carry the reversal indicator. Opening balance is zero at inception and closing balance replays from the included entries.

The Go tests verify namespaces, required structures, references and exact amount round trips. For full schema validation, use Python 3 with lxml:

```sh
python3 validation/validate_xml.py samples/seed42-2024
# For offline validation if the official XSDs have already been downloaded:
python3 validation/validate_xml.py samples/seed42-2024 --schema-directory path/to/xsds
```

The validator pins the official archived schemas by SHA-256 and disables external XML entity and network resolution during parsing. The bundled baseline and aggressive-chaos test exports both passed XSD validation against those official definitions.

## ISO 8583 card profile

The codec uses MTIs 0100 authorization, 0200 financial clearing, and 0400 full reversal. MTI and all fields are ASCII decimal. The ASCII-hex variant uses 16 uppercase characters per bitmap and the binary variant uses eight raw bytes per bitmap. The primary first bit indicates a secondary bitmap. Bytes and fields are in ascending order with bit 1 in the most significant position.

| DE | ASCII digits | Meaning |
|---:|---:|---|
| 3 | 6 | Processing code, default `000000` |
| 4 | 12 | Exact minor-unit magnitude |
| 7 | 10 | UTC `MMDDhhmmss` |
| 11 | 6 | Sequential STAN paired across lifecycle messages, wrapping after 999999 |
| 49 | 3 | ISO 4217 numeric currency |
| 90 | 42 | Original MTI (4), STAN (6), DE7 (10), acquirer identifier (11), forwarding identifier (11). Present on 0400. |

The base primary bitmap is `3220000000008000`. On 0400, it becomes `B220000000008000` followed by `0000004000000000`. The card index carries year, subsecond timestamp, canonical transaction, and associated journal ID when present. A file frame is an unsigned two-byte big-endian message length followed by one complete message. The standalone encoder emits raw messages. This is a bounded fixture profile with synthetic institution references and no cardholder fields or network-specific token, cryptogram or transport fields.

The simulator tags baseline card purchases and the bundle routes those selected lifecycles. If a selected card booking has a full canonical reversal the history serializer produces a 0400 linked to its 0200. Partial adjustments remain represented in the canonical and statement histories. The separate reversal sample selects an existing balanced Phase 4 freeze-and-reverse transaction explicitly as a card codec fixture. Each of its two streams contains one 0100, one 0200, and one 0400, with a shared index.

## COBOL batch format

The supplied [copybook](serializers/ssfd-batch.cpy) defines 200-byte fixed blocked records with no record delimiter or length prefix. Character data uses ASCII, IBM CP037, or IBM CP500. EBCDIC is emitted as direct bytes. The code pages have different byte mappings for the opening bracket in the version header, which also provides an immediate encoding check.

| Record | Fields, in wire order |
|---|---|
| Header H | Type (1), `[SSFD1]` (7), book ID (20), padding (172) |
| Detail D | Type (1), book (20), journal (20), commit sequence (20), transaction (20), original journal (20), account (20), one-based posting position (10), currency (3), journal kind (1), signed minor units (19), UTC effective time (30), padding (16) |
| Trailer T | Type (1), currency (3), detail count (20), gross debit minor units (31), gross credit minor units (31), uppercase SHA-256 (64), padding (50) |

Amounts use debit-positive signed zoned decimal. EBCDIC non-final digits use F zones, and the trailing digit uses C for positive and D for negative. ASCII uses the profile's `{ABCDEFGHI` positive and `}JKLMNOPQR` negative overpunch. In a COBOL deployment, select the compiler's arithmetic options for 31-position display numerics and the matching zoned-decimal sign convention. UTC is fixed width as `YYYY-MM-DDThh:mm:ss.nnnnnnnnnZ`. Per-currency trailers are sorted and hash the exact encoded detail records for that currency in their wire order. The final `***` trailer counts and hashes every detail and has zero monetary totals. Decode checks journal balance, identity, framing, detail order, signs, canonical padding, and all trailer bytes.

## Verification and trade-offs

Phase 5 preserves the earlier packages and their fixture digests. Seed 42 exports deterministically, with 1,872 journals, 3,866 booked postings and two reconciled currencies. Tests independently compare decoded statement, flat file and financial card amounts with canonical evidence and detect corrupted flat-file data. The resulting batch profile is exact but holds the complete snapshot and file data in memory. Deployment limits and large-run throughput are future measurements.

References:

- [ISO 20022 message archive and versioned XSDs](https://www.iso20022.org/catalogue-messages/iso-20022-messages-archive)
- [ISO 8583 profile field and bitmap terminology](https://pyiso8583.readthedocs.io/en/stable/specifications.html)
- [IBM zoned decimal constants](https://www.ibm.com/docs/en/hla-and-tf/1.6.0?topic=z-zoned-decimal-constants)
