# Inherit tokens: the owner's notice and its delivery time (#2619, ADR-0059)

**What changed.** Shared migration `0278_token_lifecycle_inherit_notice.sql`
adds the nullable `inherit_notice_delivered_at timestamptz` to
`elitea_identity.token_lifecycle`. Once the sunset date of `inherit` tokens is
set (a later release, #2579), the personal access token expiry sweep queues one
e-mail per owner of an `inherit` token, listing those tokens and the end date,
in the access notice outbox of #2519; the `main.access_notice_sender` routine
sends it and stamps the delivery time on each listed token. An `inherit` token
ends at the later of the sunset date and 30 days after that delivery, and the
token responses (`inherit_ends_on`, on `/api/v2/auth/token/` and
`/api/v2/admin/access/tokens`) state that date. The "run now" route of the
expiry sweep answers three more numbers, `inherit_owners`, `inherit_queued`
and `inherit_unreachable` (owners with no e-mail address: no notice reaches
them, so their `inherit` tokens end at the sunset date; the sweep also logs
a warning for them).

**The upgrade.** One column, no default, no data change. While the sunset date
is not set (this release) nothing is queued and nothing is read.

**Who is affected.** Owners of `inherit` tokens get the e-mail once the date is
set, so outbound e-mail should be configured by then: until a notice is
delivered, the 30 days do not start. A deployment that upgrades after the date
was set sends the notice on its first sweep. Nobody needs to act now.

**Rollback.** The previous release ignores the column.
