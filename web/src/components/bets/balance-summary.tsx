'use client';

/**
 * Cash, escrow, and the total.
 *
 * # There are two balances and reporting only one would be a lie of omission
 *
 * `cash` is spendable. `escrow` holds the stakes of open wagers, which have LEFT
 * the spendable balance but have not yet been won or lost. A customer with 100
 * staked and 20 spendable does not have 20; they have 120 of which 100 is
 * committed, and a single "balance" figure has to choose which of those two
 * facts to hide.
 *
 * # Both are DERIVED, never stored
 *
 * They are folded from `ledger_entries` through the `account_balances` view.
 * `entry_count` of zero means the account has never moved — a different fact
 * from "moved and nets to zero", and the only thing that distinguishes them, so
 * a brand-new account gets its own sentence rather than a row of zeros that
 * looks like a failed load.
 *
 * # Play money, and it says so
 *
 * The API's currency is the literal string `PLAY` and deliberately not an ISO
 * 4217 code, because labelling play money `USD` is the first step toward a
 * client treating it as money. Nothing here renders a currency symbol.
 */

import { Skeleton } from '@/components/ui';
import type { SchemaBalanceResponse } from '@/lib/api/schema';
import { MONEY_UNIT, formatMinor, spokenMinor } from '@/lib/money';

export interface BalanceSummaryProps {
  readonly balance: SchemaBalanceResponse | undefined;
  readonly loading: boolean;
}

export function BalanceSummary({ balance, loading }: BalanceSummaryProps) {
  if (balance === undefined) {
    return loading ? (
      <Skeleton className="h-16 w-full max-w-md rounded-card" />
    ) : null;
  }

  const untouched =
    balance.cash.entry_count === 0 && balance.escrow.entry_count === 0;

  return (
    <section
      aria-label="Balance"
      className="flex flex-col gap-2 rounded-card border border-rule bg-ground-1 p-4"
    >
      <dl className="flex flex-wrap gap-x-8 gap-y-3">
        <Amount
          label="Spendable"
          minor={balance.cash.balance_minor}
          tone="money"
        />
        <Amount label="In open bets" minor={balance.escrow.balance_minor} />
        <Amount label="Total" minor={balance.total_minor} />
      </dl>

      <p className="t-mono text-ink-muted">
        {untouched
          ? `${MONEY_UNIT} · this account has no ledger movement yet`
          : `${MONEY_UNIT} · derived from ${String(
              balance.cash.entry_count + balance.escrow.entry_count,
            )} ledger entries`}
      </p>
    </section>
  );
}

function Amount({
  label,
  minor,
  tone,
}: {
  readonly label: string;
  readonly minor: number;
  readonly tone?: 'money';
}) {
  return (
    <div className="flex flex-col gap-1">
      <dt className="t-label text-ink-muted">{label}</dt>
      <dd
        className={`t-price-lg tabular ${
          // Only the SPENDABLE balance is green. Escrow is money the customer
          // cannot spend and the total is a sum of the two, so tinting all three
          // would make the one number they can act on indistinguishable from the
          // two they cannot.
          tone === 'money' ? 'text-money' : 'text-ink'
        }`}
      >
        <span aria-hidden="true">{formatMinor(minor)}</span>
        <span className="sr-only">{spokenMinor(minor)}</span>
      </dd>
    </div>
  );
}
