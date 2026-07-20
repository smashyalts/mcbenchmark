# Plugin Load Testing (NexusAuctionHouse example)

The replay client can drive real plugins end-to-end — both **commands** and
**container/GUI interactions** — which is how you profile a plugin under load.
This was validated with NexusAuctionHouse on Paper 26.1.2.

## Server setup

Install into the benchmark server's `plugins/`:

| Plugin | Purpose | Source (Modrinth unless noted) |
|--------|---------|--------------------------------|
| NexusAuctionHouse | the plugin under test | `nexusauctionhouse` |
| NexusCore | **required** by NexusAuctionHouse (its main class extends a NexusCore type — despite being listed only as a soft-depend) | `nexuscore` |
| EssentialsX | economy provider (balances) | `essentialsx` |
| VaultUnlocked | Vault economy API bridge (VaultReloaded isn't published on Modrinth/Hangar; VaultUnlocked is the maintained modern fork) | `vaultunlocked` |

Server config: offline-mode, `gamemode=creative` (lets the replay set inventory
via creative-set), and the demo users in `ops.json` (op level 4).

> **This example is a synthetic isolation test, not a realistic benchmark.** It
> starts from a blank world with zero balances and one hand-written listing, so
> the buyer grants itself money with `/eco give` and the seller creative-sets the
> item. A *real* capture-replay run does neither: the benchmark server is seeded
> from a snapshot of the production/capture server (world + `playerdata` +
> economy store, e.g. Essentials `userdata/` or its SQL/Redis backend), so
> replayed players already hold their real balances and items, and whatever they
> did to earn/spend is in the traces and re-executes. Treat `/eco give` and
> creative-set here as scaffolding to exercise the plugin's code paths in
> isolation, not as part of the load model.

## Discovering the plugin's interface

No manual play needed — read it from the jar:

- **Commands**: `/ah` opens the GUI, `/ah sell <price>` lists the held item.
  (Aikar-ACF `@CommandAlias`/`@Subcommand` values are in the command class's
  constant pool — `javap -c`.)
- **GUI slots**: the confirm button in ConfirmSell/ConfirmBuyGui is at Triumph
  `(row 3, col 3)` → container slot **20**. MainGui fills the left column and
  bottom row as borders, so the first listing sits at slot **1**.

## The replay flow

`gen-demo --ah --price 100` writes two ordered traces:

- **Seller** (`DEMO_00000`): creative-set diamonds into hand → `/ah sell 100`
  (command) → click slot 20 in ConfirmSellGui (container) → item listed.
- **Buyer** (`DEMO_00001`): `/eco give <self> <amount>` to fund itself (command;
  works because the buyer is op'd → has `essentials.eco`) → `/ah` (command) →
  click slot 1 in MainGui (container) → click slot 20 in ConfirmBuyGui
  (container) → purchase.

The buyer's self-funding amount and name are `gen-demo` flags
(`--buyer-money`, `--buyer-name`); this keeps balances out of server config.

The buyer never uses the captured window id — each `open_screen` from the server
updates the live window id, so the MainGui→ConfirmBuyGui transition is automatic.
`reuse_policy: once` plays each trace a single time.

## Verified result

Starting the buyer at **0** balance (to prove `/eco give` funds it) and the
seller at 1000:

- Buyer **0 → 99895**: `/eco give` grants 100000, then −100 price −5 buy tax.
- Seller **1000 → 1095**: −5 list tax, +100 sale proceeds.
- Auction listings **1 → 0** (sold), diamonds delivered to the buyer's inventory
  NBT, **zero server exceptions**.

(An earlier run with both at a 100000 starting balance gave seller 100095 / buyer
99895 — same deltas.)

Every step was driven by the replay client's packets through the plugin's own
command and container handlers plus the Vault/Essentials economy — the basis for
scaling to hundreds of concurrent AH interactions to profile the plugin (Paper
bundles `/spark` for tick/timings analysis).
