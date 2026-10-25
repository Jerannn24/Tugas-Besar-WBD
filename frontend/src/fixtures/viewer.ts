import type { Column, Constraint, ID, Index, SchemaSummary, TableMetadata } from '../api/types'
import { TARGETS } from './data'

export type FixtureTable = { meta: TableMetadata; rows: (string | null)[][] }

export type FixtureDatabase = {
  schemas: SchemaSummary[]
  tables: Map<string, FixtureTable>
  unavailableTables: Set<string>
  failFirstSchemas: boolean
}

export const tableKey = (schema: string, table: string) => `${schema}\u0000${table}`

function col(name: string, data_type: string, nullable = true, default_expression: string | null = null): Column {
  return { name, data_type, nullable, default_expression }
}

function table(
  schema: string,
  name: string,
  columns: Column[],
  constraints: Constraint[],
  indexes: Index[],
  rows: (string | null)[][],
): FixtureTable {
  return { meta: { schema, table: name, columns, constraints, indexes }, rows }
}

function buildDatabase(
  tables: FixtureTable[],
  extraSchemas: string[] = [],
  options: { unavailable?: [string, string][]; failFirstSchemas?: boolean } = {},
): FixtureDatabase {
  const bySchema = new Map<string, { name: string }[]>()
  for (const t of tables) {
    const list = bySchema.get(t.meta.schema) ?? []
    list.push({ name: t.meta.table })
    bySchema.set(t.meta.schema, list)
  }
  for (const [schema, name] of options.unavailable ?? []) {
    const list = bySchema.get(schema) ?? []
    list.push({ name })
    bySchema.set(schema, list)
  }
  for (const s of extraSchemas) if (!bySchema.has(s)) bySchema.set(s, [])

  return {
    schemas: [...bySchema.entries()].map(([name, tbls]) => ({ name, tables: tbls })),
    tables: new Map(tables.map((t) => [tableKey(t.meta.schema, t.meta.table), t])),
    unavailableTables: new Set((options.unavailable ?? []).map(([s, n]) => tableKey(s, n))),
    failFirstSchemas: options.failFirstSchemas ?? false,
  }
}

// ---------- demo_main ----------

const customers = table(
  'public',
  'customers',
  [
    col('id', 'bigint', false, "nextval('customers_id_seq'::regclass)"),
    col('email', 'text', false),
    col('full_name', 'text', false),
    col('status', 'text', false, "'active'::text"),
    col('note', 'text'),
    col('balance', 'numeric(38,9)', false, '0'),
    col('profile', 'jsonb'),
    col('created_at', 'timestamp with time zone', false, 'now()'),
  ],
  [
    { name: 'customers_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY (id)' },
    { name: 'customers_email_key', kind: 'UNIQUE', definition: 'UNIQUE (email)' },
    {
      name: 'customers_status_check',
      kind: 'CHECK',
      definition: "CHECK ((status = ANY (ARRAY['active'::text, 'suspended'::text, 'closed'::text])))",
    },
  ],
  [
    {
      name: 'customers_pkey',
      definition: 'CREATE UNIQUE INDEX customers_pkey ON public.customers USING btree (id)',
      is_unique: true,
      is_primary: true,
    },
    {
      name: 'customers_email_key',
      definition: 'CREATE UNIQUE INDEX customers_email_key ON public.customers USING btree (email)',
      is_unique: true,
      is_primary: false,
    },
    {
      name: 'customers_created_at_idx',
      definition: 'CREATE INDEX customers_created_at_idx ON public.customers USING btree (created_at DESC)',
      is_unique: false,
      is_primary: false,
    },
  ],
  [
    ['1', 'ana@example.test', 'Ana Putri', 'active', null, '1250.500000000', '{"tier": "gold", "tags": ["vip", "early"]}', '2026-01-04 08:15:00+00'],
    ['2', 'budi@example.test', 'Budi Santoso', 'active', 'NULL', '0.000000000', null, '2026-02-11 13:02:45.123456+00'],
    ['3', 'citra@example.test', 'Citra "CJ" Jaya', 'suspended', '', '12345678901234567890.123456789', '{}', '2026-03-20 23:59:59+00'],
    ['4', 'dewi@example.test', 'Dewi <b>tidak bold</b>', 'closed', 'Catatan panjang: ' + 'lorem ipsum dolor sit amet '.repeat(12), '-0.000000001', '{"address": {"city": "Bandung", "zip": "40132", "lines": ["Jl. Ganesha 10", "Lebak Siliwangi"]}, "consents": {"email": true, "sms": false}}', '2026-04-01 00:00:00+00'],
    ['9223372036854775807', 'max@example.test', 'Max Bigint', 'active', 'baris dengan id bigint maksimum', '99999999999999999999999999999.999999999', null, '2026-05-05 05:05:05+00'],
  ],
)

const orders = table(
  'public',
  'orders',
  [
    col('id', 'bigint', false, "nextval('orders_id_seq'::regclass)"),
    col('customer_id', 'bigint', false),
    col('amount_cents', 'bigint', false),
    col('currency', 'character(3)', false, "'IDR'::bpchar"),
    col('placed_at', 'timestamp without time zone', false, 'now()'),
    col('shipped_on', 'date'),
  ],
  [
    { name: 'orders_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY (id)' },
    {
      name: 'orders_customer_id_fkey',
      kind: 'FOREIGN KEY',
      definition: 'FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE RESTRICT',
    },
    { name: 'orders_amount_check', kind: 'CHECK', definition: 'CHECK ((amount_cents <> 0))' },
  ],
  [
    {
      name: 'orders_pkey',
      definition: 'CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)',
      is_unique: true,
      is_primary: true,
    },
    {
      name: 'orders_customer_id_idx',
      definition: 'CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id)',
      is_unique: false,
      is_primary: false,
    },
  ],
  [
    ['1', '1', '9223372036854775807', 'IDR', '2026-06-01 10:00:00', '2026-06-03'],
    ['2', '1', '-9223372036854775808', 'IDR', '2026-06-02 11:30:00', null],
    ['3', '2', '9007199254740993', 'USD', '2026-06-03 12:45:10.5', '2026-06-05'],
    ['4', '9223372036854775807', '1', 'EUR', '2026-06-04 09:00:00', null],
  ],
)

const events = table(
  'public',
  'events',
  [
    col('id', 'bigint', false, "nextval('events_id_seq'::regclass)"),
    col('kind', 'text', false),
    col('payload', 'json'),
    col('occurred_at', 'timestamp with time zone', false, 'now()'),
  ],
  [{ name: 'events_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY (id)' }],
  [
    {
      name: 'events_pkey',
      definition: 'CREATE UNIQUE INDEX events_pkey ON public.events USING btree (id)',
      is_unique: true,
      is_primary: true,
    },
  ],
  Array.from({ length: 137 }, (_, i) => {
    const n = i + 1
    const kinds = ['login', 'logout', 'view', 'export']
    const minute = String(n % 60).padStart(2, '0')
    const hour = String(Math.floor(n / 60)).padStart(2, '0')
    return [
      String(n),
      kinds[n % kinds.length],
      n % 10 === 0 ? null : `{"seq": ${n}, "ok": ${n % 3 !== 0}}`,
      `2026-07-01 ${hour}:${minute}:00+00`,
    ]
  }),
)

const auditLog = table(
  'public',
  'audit_log',
  [
    col('id', 'bigint', false, "nextval('audit_log_id_seq'::regclass)"),
    col('actor', 'text', false),
    col('action', 'text', false),
    col('logged_at', 'timestamp with time zone', false, 'now()'),
  ],
  [{ name: 'audit_log_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY (id)' }],
  [
    {
      name: 'audit_log_pkey',
      definition: 'CREATE UNIQUE INDEX audit_log_pkey ON public.audit_log USING btree (id)',
      is_unique: true,
      is_primary: true,
    },
  ],
  [],
)

const orderItems = table(
  'Sales Data',
  'Order Items',
  [
    col('Item ID', 'integer', false),
    col('Unit Price', 'numeric(12,2)', false),
    col('qty', 'smallint', false, '1'),
    col('select', 'text'),
  ],
  [
    { name: 'Order Items_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY ("Item ID")' },
    { name: 'Order Items_qty_check', kind: 'CHECK', definition: 'CHECK ((qty > 0))' },
  ],
  [
    {
      name: 'Order Items_pkey',
      definition: 'CREATE UNIQUE INDEX "Order Items_pkey" ON "Sales Data"."Order Items" USING btree ("Item ID")',
      is_unique: true,
      is_primary: true,
    },
    {
      name: 'order_items_unit_price_idx',
      definition: 'CREATE INDEX order_items_unit_price_idx ON "Sales Data"."Order Items" USING btree ("Unit Price")',
      is_unique: false,
      is_primary: false,
    },
  ],
  [
    ['1', '19999.99', '2', 'kolom bernama kata kunci "select"'],
    ['2', '0.01', '1', null],
    ['3', '1000000000.00', '5', 'NULL'],
  ],
)

// ---------- demo_second ----------

const products = table(
  'inventory',
  'products',
  [
    col('sku', 'text', false),
    col('name', 'text', false),
    col('stock', 'integer', false, '0'),
    col('updated_at', 'timestamp with time zone', false, 'now()'),
  ],
  [
    { name: 'products_pkey', kind: 'PRIMARY KEY', definition: 'PRIMARY KEY (sku)' },
    { name: 'products_stock_check', kind: 'CHECK', definition: 'CHECK ((stock >= 0))' },
  ],
  [
    {
      name: 'products_pkey',
      definition: 'CREATE UNIQUE INDEX products_pkey ON inventory.products USING btree (sku)',
      is_unique: true,
      is_primary: true,
    },
  ],
  [
    ['SKU-001', 'Kabel UTP 5m', '120', '2026-08-01 07:00:00+00'],
    ['SKU-002', 'Switch 8 port', '0', '2026-08-02 07:00:00+00'],
    ['SKU-003', 'Router', '7', '2026-08-03 07:00:00+00'],
  ],
)

export const VIEWER: Record<ID, FixtureDatabase> = {
  [TARGETS.demo_main.id]: buildDatabase([customers, orders, events, auditLog, orderItems]),
  [TARGETS.demo_second.id]: buildDatabase([products], ['staging'], {
    unavailable: [['inventory', 'remote_snapshot']],
    failFirstSchemas: true,
  }),
}
