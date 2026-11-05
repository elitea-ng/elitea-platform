# Platform Ops weekly — Tuesday 10 March 2026

**Present:** Priya Natarajan (chair), Tom Becker, Marta Kowalska, Daniel Osei
**Apologies:** Hannah Lee
**Notes:** Marta

---

## 1. Vendor consolidation for observability

We are paying for three overlapping tools (two log vendors, one APM). Contracts
for the second log vendor renew on 30 June, so a decision is needed before May.
Daniel pointed out that last year's attempt stalled because nobody owned it.

- ACTION: Tom to draft a shortlist of two or three vendors with a cost
  comparison by Friday.

Priya wants the comparison to use list prices, not the discounted quote from
last year (that one was 18% off and expires anyway).

## 2. Capacity planning

Marta showed the Q1 numbers: storage is at 71% of provisioned, compute peaks
at 64%. The spreadsheet still has the old node prices from 2025.

- ACTION: Marta to update the capacity spreadsheet with current node prices
  and the new reserved-instance terms within 10 days.

- DECISION: we keep the 75% storage threshold as the trigger for ordering
  more disks; no change to the compute threshold.

## 3. SSO certificate renewal

The IdP signing certificate renewal came up again. Nobody is sure whether the
security team or Platform Ops owns it this year.

- ACTION: someone to find out who owns the SSO certificate renewal — owner
  TBD, no date set yet.

## 4. Q2 budget

Finance needs our Q2 numbers ahead of the board meeting on 24 March. Daniel
has the template from Finance.

- ACTION: Daniel to prepare the Q2 budget deck before the board meeting on
  24 March.

Proposal to move the budget review to a monthly cadence: deferred, revisit
in April. (Not a decision.)

## 5. Team offsite

- ACTION: Marta to book a venue for the team offsite by 2 April.

- DECISION: offsite format is one day, in person, no external facilitator.

## AOB

Tom mentioned he will be on parental leave from next Monday for three weeks.

---

### Corrections added after the meeting (Priya, 11 March)

1. Because Tom starts his leave on Monday, Priya takes over the vendor
   shortlist from Tom. Same deadline.
2. The offsite has been cancelled for this quarter (budget freeze), so the
   venue booking is dropped. The format decision stands for whenever we do
   hold it.

**Next sync:** Tuesday 17 March 2026, 15:00 CET (Berlin), same video link.
