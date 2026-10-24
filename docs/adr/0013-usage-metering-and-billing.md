# ADR 0013: Usage Metering and Billing

- Status: Deferred (requirements recorded; quota enforcement is not billing)
- Date: 2026-10-24
- Owner: Hako Product and Platform Operations
- Scope: Future customer usage records, cost attribution, and invoicing.

## Context

Hako enforces Tenant quotas for Workspaces, Workload Runs, Previews, artifacts,
and Workspace Services. These controls protect capacity and limit admitted
usage; they do not establish billable usage, AWS cost attribution, prices,
credits, invoices, or payment state.

## Decision

Keep quota/usage UX separate from billing. Do not present current quota counters
as an invoice or monetary estimate. A future billing system must derive
idempotent usage records from durable lifecycle events and distinguish
allocated, active, suspended, retained-storage, and cleanup-pending resources.
Each record must be attributable to a Tenant, Resource Plane, Region, resource
class, and bounded time interval without collecting source code or terminal
contents.

Pricing, rounding, free tiers, credits, taxes, currency, invoice corrections,
and payment collection remain product/business decisions and are not encoded
in runtime or quota behavior.

## Requirements before implementation

- Name an accountable product/billing owner and define supported customer
  plans, currencies, invoicing, tax, and payment-provider requirements.
- Define authoritative meter events, clock rules, duplicate correction,
  deletion/reconciliation, and immutable audit retention.
- Define how Lambda MicroVM duration, EFS storage, artifacts, Gateway/network,
  and managed databases map to usage without double-counting retries.
- Specify estimate visibility, export, invoice dispute, and support workflows.
- Validate cost attribution against AWS billing data in an isolated account
  before charging customers.

## Non-goals

- Charging customers based only on configured quotas or API request counts.
- Storing payment-card data in Hako.
- Enabling paid usage, invoice generation, or payment collection in this ADR.
- Claiming that AWS resource tags alone are a complete usage ledger.

## Revisit when

Product selects plans and an accountable billing owner, and measured Resource
Plane lifecycle events can be reconciled against AWS cost data. This ADR does
not authorize customer charges or AWS deployment.
