# Marketplace: feedback per listing version (ADR-0051 M5, issue 1631)

**What changed.** A new shared migration adds feedback to the Marketplace schema:
`listing_feedback` (reviews, workforce reports), its audit trail, `listing_signals_daily`,
and a `dismissed_at` column on `elitea_marketplace.installs`. The number is `0252` in the pull
request and is renumbered above the head of `main` at merge. Client contract 1.55 adds two read
operations to the client set (`searchMarketplaceListings`, `getMarketplaceFeedback`) for the
mobile agent sheet. Deleting a hired agent now marks its hire dismissed, so the hires count
and the dismissal rate of its listing stay true. The admin published agents view now fills
`adoption.project_count` from active hires.

**Who is affected.** Every operator. Existing hires stay active. No backfill is needed.

**What to do.** Run the migrations, then upgrade Main and the clients together. No other
action is required.

A new routine, `main.marketplace_signals`, runs once a day and computes the listing signals
and refreshes the ratings. It pauses in maintenance. You can switch it off or change its
schedule on **Schedules & Tasks**.
