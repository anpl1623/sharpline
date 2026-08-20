'use client';

/**
 * The book filter. Multi-select over `GET /books`, persisted in the preferences
 * store, fed straight into every board request as a repeatable `book` parameter.
 *
 * EMPTY MEANS EVERY BOOK. It does not mean "no books", and the trigger says
 * "All books" rather than "0 selected" so that reading cannot go wrong.
 *
 * # Two tags, and both are obligations rather than decoration
 *
 * `synthetic` — ADR 0003 and the OpenAPI spec both require it: the in-house book
 * is a stochastic market maker, so a quote from it is a statement about a random
 * number generator, and EVERY surface that renders one must be able to say so.
 * A synthetic price that looks like an observed price is the single most
 * misleading thing this product could show.
 *
 * `reference` — the sharp book the pricer devigs against. Every expected-value,
 * edge and Kelly number in the system is measured relative to it, so a user
 * comparing books has to be able to see which one the ruler is.
 *
 * Neither tag relies on its colour: both spell the word.
 *
 * # No fabricated rows
 *
 * If `/books` fails, the menu says so and shows the request id. If it returns an
 * empty catalogue, the menu says the catalogue is empty. Neither case invents a
 * book to fill the space.
 */

import { useQuery } from '@tanstack/react-query';
import { Filter } from 'lucide-react';

import {
  Badge,
  Button,
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Skeleton,
} from '@/components/ui';
import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { booksQueryOptions } from '@/lib/api/queries';
import type { SchemaBook } from '@/lib/api/schema';
import {
  useBookFilter,
  useClearBookFilter,
  useToggleBook,
} from '@/lib/store/preferences';
import { cn } from '@/lib/utils';

/** Stable identity so an empty catalogue does not churn referentially. */
const NO_BOOKS: readonly SchemaBook[] = [];

export interface BookFilterProps {
  readonly className?: string | undefined;
}

export function BookFilter({ className }: BookFilterProps) {
  const { data, isPending, isError, error } = useQuery(booksQueryOptions());
  const selected = useBookFilter();
  const toggleBook = useToggleBook();
  const clearBookFilter = useClearBookFilter();

  const books = data?.data ?? NO_BOOKS;
  const showingAll = selected.length === 0;

  const triggerLabel = showingAll
    ? 'All books'
    : `${String(selected.length)} book${selected.length === 1 ? '' : 's'}`;

  const triggerName = showingAll
    ? 'Book filter: all books'
    : `Book filter: ${String(selected.length)} of ${String(books.length)} books selected`;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          size="sm"
          variant="default"
          aria-label={triggerName}
          className={cn('shrink-0 gap-1.5 px-2', className)}
        >
          <Filter className="size-4" aria-hidden="true" />
          <span className="hidden lg:inline">{triggerLabel}</span>
          {showingAll ? null : (
            <span className="lg:hidden t-label text-ink">
              {String(selected.length)}
            </span>
          )}
        </Button>
      </DropdownMenuTrigger>

      <DropdownMenuContent align="end" className="w-64">
        <DropdownMenuLabel>Books</DropdownMenuLabel>

        <DropdownMenuCheckboxItem
          checked={showingAll}
          /* Keep the menu open: this is a multi-select, and closing it after
           * every click would make selecting three books three round trips. */
          onSelect={(event) => {
            event.preventDefault();
          }}
          onCheckedChange={() => {
            clearBookFilter();
          }}
        >
          <span className="text-ink">All books</span>
        </DropdownMenuCheckboxItem>

        <DropdownMenuSeparator />

        {isPending ? (
          <div className="flex flex-col gap-1 p-1" aria-hidden="true">
            <Skeleton className="h-8 w-full rounded-price" />
            <Skeleton className="h-8 w-full rounded-price" />
            <Skeleton className="h-8 w-full rounded-price" />
          </div>
        ) : null}

        {isError ? (
          <div className="flex flex-col gap-1 px-2 py-2">
            <p className="t-ui text-loss">{userFacingMessage(error)}</p>
            {developerDetail(error) === null ? null : (
              <p className="t-mono text-ink-muted">{developerDetail(error)}</p>
            )}
          </div>
        ) : null}

        {!isPending && !isError && books.length === 0 ? (
          <p className="px-2 py-2 t-ui text-ink-muted">
            No books in the catalogue.
          </p>
        ) : null}

        {books.map((book) => (
          <DropdownMenuCheckboxItem
            key={book.slug}
            checked={selected.includes(book.slug)}
            onSelect={(event) => {
              event.preventDefault();
            }}
            onCheckedChange={() => {
              toggleBook(book.slug);
            }}
          >
            <span className="min-w-0 flex-1 truncate">{book.name}</span>
            {book.is_reference ? (
              <Badge variant="info" title="Fair value is devigged from this book">
                reference
              </Badge>
            ) : null}
            {book.kind === 'synthetic' ? (
              <Badge
                variant="neutral"
                title="Quotes generated by this system's stochastic market maker"
              >
                synthetic
              </Badge>
            ) : null}
          </DropdownMenuCheckboxItem>
        ))}

        <DropdownMenuSeparator />

        <p className="px-2 pt-1 pb-2 t-mono text-ink-muted">
          A synthetic book&rsquo;s quotes are computed by this system, not
          observed. EV is measured against the reference book.
        </p>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
