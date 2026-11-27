# Tugas Besar Milestone 1 - Web Based Development

## Deskripsi Singkat

Repositori ini berisi implementasi platform web untuk mengelola database PostgreSQL secara terpusat: admin mendaftarkan koneksi database target, mengatur user, grup, dan hak akses, lalu user dapat melihat data dan menyiapkan serta mengeksekusi migrasi SQL dengan riwayat yang tercatat.

## Tech Stack

| Bagian   | Tech                                                             |
| -------- | ---------------------------------------------------------------- |
| Backend  | Go 1.26, Fiber v2, pgx v5                                        |
| Frontend | React 19, TypeScript, Vite 8 (multi-page app), Tabler UI         |
| Database | PostgreSQL 16                                                    |
| Runtime  | Docker Compose (layanan `frontend`, `backend`, `postgres`, `postgres-test`) |

## Struktur Repository

```
.
├── backend/              # API Go (Fiber); juga menyajikan hasil build frontend
│   ├── modules/          # auth, authz, admin, connections, databaseapi
│   ├── migration/        # preflight, eksekusi, dan recovery migrasi target
│   ├── history/ drafts/  # riwayat eksekusi dan draft migrasi
│   └── main.go
├── frontend/             # React + Vite, satu folder per halaman (login/, databases/, admin/...)
│   └── src/              # api/, components/, pages/, fixtures/ (data dummy)
├── db/
│   ├── init/             # provisioning role & database saat volume Postgres pertama dibuat
│   ├── migrations/       # migrasi database platform (dijalankan lewat db/apply.sh)
│   ├── target-profile/   # profil akses database target (role executor/preparer/viewer)
│   └── README.md         # detail teknis database
├── docker-compose.yaml
└── .env.example          # template konfigurasi
```

## Prasyarat

**Wajib**

- [Git](https://git-scm.com/)
- [Docker](https://docs.docker.com/get-docker/) dengan **Docker Compose v2** (perintah `docker compose`, bukan `docker-compose`)
- [Node.js](https://nodejs.org/) LTS ≥ 20.19 (untuk build frontend; alternatif tanpa Node.js ada di langkah 1)
- **Bash** dan **openssl** (untuk menjalankan skrip di `db/` dan membuat sandi)

> **Windows:** jalankan semua perintah dari WSL2 (disarankan) atau Git Bash. Jika memakai Docker Desktop, aktifkan *WSL Integration* untuk distro yang dipakai.

Semua perintah di bawah dijalankan dari **root repository**, kecuali disebutkan lain.

## Konfigurasi `.env`

### 1. Salin template

```sh
cp .env.example .env
```

### 2. Isi semua sandi database

Setiap variabel `*_PASSWORD` untuk database **wajib** berupa **64 karakter heksadesimal yang unik**, tanpa tanda kutip. Skrip provisioning akan menolak nilai lain. Perintah ini mengisi semua sandi database yang masih kosong sekaligus:

```sh
awk 'BEGIN{FS=OFS="="}
     /^DB_.*_PASSWORD=$/ { cmd="openssl rand -hex 32"; cmd | getline v; close(cmd); $2=v }
     { print }' .env > .env.tmp && mv .env.tmp .env
```

### 3. Ganti `JWT_SECRET`

Nilai di template hanya contoh. Buat nilai baru:

```sh
openssl rand -base64 32
```

lalu tempel hasilnya ke `JWT_SECRET=` di `.env`. Secret ini juga dipakai untuk mengenkripsi kredensial koneksi yang disimpan, jadi **jangan diganti** setelah ada koneksi yang tersimpan.

### 4. Isi akun admin pertama

```env
ADMIN_BOOTSTRAP_USERNAME=admin
ADMIN_BOOTSTRAP_PASSWORD=GantiSandiIni#2026
```

Sandi admin minimal 12 karakter dan harus memuat huruf besar, huruf kecil, angka, dan simbol. Akun dibuat saat backend start. Jika username sudah ada, akun tidak diubah.

### Arti variabel

| Variabel                                   | Keterangan                                                               |
| ------------------------------------------ | ------------------------------------------------------------------------ |
| `COMPOSE_PROJECT_NAME`                     | Prefix nama container dan volume Docker                                  |
| `DB_DEV_*` / `DB_TEST_*`                   | Konfigurasi Postgres development (`postgres`) dan test (`postgres-test`) |
| `*_BOOTSTRAP_USER/PASSWORD`                | Superuser Postgres, hanya untuk provisioning dan profil target          |
| `*_OWNER_USER/PASSWORD`                    | Pemilik schema platform (`wbd_schema_owner`), dipakai `db/apply.sh`      |
| `*_RUNTIME_USER/PASSWORD`                  | Role yang dipakai backend (`wbd_platform_runtime`)                       |
| `*_PLATFORM_DB`                            | Database internal aplikasi (user, sesi, draft, riwayat)                  |
| `*_TARGET_A` / `*_TARGET_B`                | Dua database contoh yang dikelola lewat aplikasi                         |
| `*_TARGET_{A,B}_{EXECUTOR,PREPARER,VIEWER}_PASSWORD` | Sandi role akses database target                               |
| `JWT_SECRET`                               | Kunci enkripsi sesi dan kredensial koneksi                               |
| `ADMIN_BOOTSTRAP_USERNAME/PASSWORD`        | Admin platform pertama; kosongkan keduanya untuk melewati                |

> Nama role (`wbd_schema_owner`, `wbd_platform_runtime`) dan nama database jangan diubah, karena skrip memeriksa nilai tersebut.

## Menjalankan Aplikasi

Langkah berikut **harus berurutan**.

> Jangan menjalankan `docker compose up -d` tanpa nama service: backend akan start sebelum migrasi lalu berhenti. Service `frontend` hanya untuk mode development.

### 1. Build frontend

Backend menyajikan hasil build dari `frontend/dist`. Build dulu **sebelum** menjalankan container backend. Jika folder belum ada, Docker akan membuatnya sebagai folder kosong milik root.

```sh
cd frontend
npm ci
npm run build
cd ..
```

Tanpa Node.js di host:

```sh
docker compose run --rm --no-deps frontend npm run build
```

### 2. Jalankan PostgreSQL

```sh
docker compose up -d postgres
```

Saat volume pertama kali dibuat, `db/init/10-provision.sh` membuat role serta database platform dan target. Tunggu sampai status `healthy`:

```sh
docker compose ps postgres
```

PostgreSQL development mengaktifkan `track_commit_timestamp` agar worker dapat memverifikasi waktu commit di database target. Target PostgreSQL eksternal juga perlu mengaktifkan pengaturan ini sebelum menjalankan migrasi.

### 3. Terapkan migrasi platform

```sh
bash db/apply.sh dev
```

Output yang diharapkan: `PASS dev platform migrations applied or already current`. Perintah ini aman dijalankan ulang.

### 4. Terapkan profil akses database target

```sh
bash db/target-profile/apply.sh dev a
bash db/target-profile/apply.sh dev b
```

Perintah ini membuat schema `managed` dan role `wbd_target_<a|b>_executor`, `_preparer`, `_viewer` di setiap database target.

### 5. Jalankan backend

```sh
docker compose up -d backend
docker compose logs -f backend   # tunggu "Server starting on port 8080", lalu Ctrl+C
```

Backend memerlukan migrasi platform `014_linear_job_sources` sebelum membuka HTTP. Dua worker antrean memungkinkan target yang berbeda diproses bersamaan; eksekusi lama dengan hasil belum pasti tetap memblokir targetnya. Saat menerima `SIGTERM` (misalnya dari `docker compose stop backend`) atau `SIGINT`, backend berhenti menerima trafik, memberi pekerjaan aktif waktu untuk selesai, lalu menutup pool database. Pekerjaan persisten yang belum selesai diperiksa kembali saat backend menyala.

### 6. Buka aplikasi

Buka **http://localhost:8080**, lalu login dengan akun dari `ADMIN_BOOTSTRAP_USERNAME` / `ADMIN_BOOTSTRAP_PASSWORD`.

### Ringkasan (setelah `.env` siap)

```sh
(cd frontend && npm ci && npm run build)
docker compose up -d postgres
bash db/apply.sh dev
bash db/target-profile/apply.sh dev a
bash db/target-profile/apply.sh dev b
docker compose up -d backend
```

Untuk menjalankan lagi di hari berikutnya, cukup `docker compose up -d postgres backend`. Migrasi dan profil hanya perlu diulang jika ada file migrasi baru.

## Penggunaan Pertama

1. **Admin → Connections**: tambahkan database target. Contoh untuk target A:

   | Field              | Nilai                                                  |
   | ------------------ | ------------------------------------------------------ |
   | Host               | `postgres`                                             |
   | Port               | `5432`                                                 |
   | Database           | `dev_target_a`                                         |
   | SSL mode           | `disable`                                              |
   | Username / Password PostgreSQL (viewer) | `wbd_target_a_viewer` / `DB_DEV_TARGET_A_VIEWER_PASSWORD` |
   | Executor           | `wbd_target_a_executor` / `DB_DEV_TARGET_A_EXECUTOR_PASSWORD` |
   | Preparer           | `wbd_target_a_preparer` / `DB_DEV_TARGET_A_PREPARER_PASSWORD` |

   Host bernilai `postgres` karena koneksi dibuat dari dalam container backend. Untuk target B, ganti `a` menjadi `b`.
2. **Admin → Users**: buat akun user.
3. **Admin → Groups**: buat grup, tambahkan anggota, lalu beri grant ke database.
4. **Databases**: lihat schema dan isi tabel.
5. **Migrations**: buat draft, lanjutkan ke review dan approval, lalu gunakan **Execute** di SPA. Tombol ini memanggil `POST /api/databases/{database_id}/proposals/{proposal_id}/jobs` dan menampilkan job. Eksekusi langsung dari draft tidak lagi tersedia.

   Riwayat mendukung pagination dengan `?limit=20` dan parameter opsional `hide_rollback=true`; tanpa query, respons memakai bentuk legacy. Kelayakan rollback dan forward sudah ditentukan di backend, tetapi kontrol rollback dan forward pada SPA saat ini masih merupakan handoff frontend.

## Testing dan Quality Check

### Frontend

```sh
cd frontend
npm run typecheck
npm run lint
npm run build
```

### Backend (unit test)

```sh
cd backend
go test ./...
```

Tanpa Go di host:

```sh
docker compose run --rm --no-deps backend go test ./...
```

Test integrasi otomatis di-*skip* jika variabel lingkungannya tidak di-set.

### Backend (test integrasi)

Test integrasi memakai layanan `postgres-test` yang terpisah dari data development.

```sh
docker compose --profile test up -d postgres-test
bash db/apply.sh test
bash db/target-profile/apply.sh test a
```

Port Postgres tidak dibuka ke host, jadi jalankan test dari container di jaringan Compose dengan variabel yang dibutuhkan, misalnya:

```sh
docker compose --profile test run --rm --no-deps \
  -e AUTH_TEST_DATABASE_URL="postgres://wbd_platform_runtime:<DB_TEST_RUNTIME_PASSWORD>@postgres-test:5432/test_platform?sslmode=disable" \
  backend go test ./modules/auth/...
```

### Bukti penerimaan performa

Pada PostgreSQL 16.15 lokal terisolasi (target B), tabel `managed.batch_items` berisi 1.000.000 baris; dataset yang sama di-reset ke `processed=false` antara pengujian. Pengaturan: `shared_buffers=128MB`, `synchronous_commit=on`, dan default `fsync=on`.

- Satu `UPDATE` tanpa batch: 8,524 detik.
- Job batch yang disetujui, ukuran 10.000: 61,371 detik dari masuk antrean hingga teramati selesai; 100 batch / 1.000.000 baris di-commit.

Batch menyediakan progres dan checkpoint, bukan klaim peningkatan kecepatan.

## Referensi Port dan Variabel

| URL                         | Layanan                                       |
| --------------------------- | --------------------------------------------- |
| http://localhost:8080       | Backend + frontend hasil build (**utama**)    |
| http://localhost:8080/api   | REST API                                      |
| http://localhost:5173       | Vite dev server (mode development frontend)   |
| `postgres:5432`             | PostgreSQL dev (hanya dari jaringan Compose)  |
| `postgres-test:5432`        | PostgreSQL test (hanya dari jaringan Compose) |

## Menghentikan dan Reset

```sh
docker compose down                     # hentikan container, data tetap tersimpan
docker compose --profile test down      # termasuk postgres-test
docker compose --profile test down -v   # hapus container DAN semua data database
```

> Mengubah sandi di `.env` **tidak** mengubah sandi database yang sudah dibuat. Jika sandi diubah, reset dengan `down -v`, lalu ulangi langkah 2–5.

## Author

- Junior Natra Situmorang - 13524055
- Ishaq Irfan Farizal - 13524094
- Reynard Nathanael - 13524103

## Bonus Observability 

Implementasi observability menyediakan tiga jenis telemetry untuk backend:

1. **Logs**: structured JSON logs ditulis ke stdout dan dikirim ke OpenTelemetry Collector melalui OTLP/HTTP. Collector meneruskan logs ke Loki.
2. **Traces**: trace HTTP request, operasi worker, dan query PostgreSQL dikirim ke Collector melalui OTLP/HTTP, lalu diteruskan ke Tempo.
3. **Metrics**: backend menyediakan endpoint Prometheus pada `/api/metrics`. Prometheus melakukan scrape setiap 15 detik dan Grafana menampilkan dashboard yang diprovisikan otomatis.

Stack observability terdiri dari OpenTelemetry Collector, Prometheus, Loki, Tempo, dan Grafana. Konfigurasi masing-masing service berada di `deploy/observability/` dan didefinisikan di `docker-compose.yaml`.

### Prasyarat

Selesaikan konfigurasi `.env`, build frontend, provisioning PostgreSQL, migrasi platform, dan profil database target sesuai bagian [Menjalankan Aplikasi](#menjalankan-aplikasi). Backend memerlukan migrasi platform `014_linear_job_sources` sebelum dapat berjalan.

Pastikan `.env` sudah tersedia dan berisi konfigurasi database serta `JWT_SECRET`. Endpoint OTLP untuk backend sudah dikonfigurasi pada service `backend` di Compose:

- `OTEL_SERVICE_NAME=dbplatform-backend`
- `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318`

Tidak perlu menambahkan endpoint Collector ke `.env` untuk penggunaan Compose standar.

### Menjalankan stack observability

Jika aplikasi dan migrasi sudah siap, jalankan service backend dan observability:

```sh
docker compose up -d --build backend prometheus loki tempo otel-collector grafana
docker compose ps
docker compose logs -f --tail 50 backend otel-collector
```

Untuk melihat structured logs backend secara langsung tanpa membuka Grafana:

```sh
docker compose logs -f backend
```

Untuk menghentikan tampilan log, tekan `Ctrl+C`; container tetap berjalan.

### Endpoint dan akses

| Komponen | URL | Fungsi |
|---|---|---|
| Backend metrics | http://localhost:8080/api/metrics | Metrics exposition format Prometheus |
| Prometheus | http://localhost:9090 | Query dan status scrape metrics |
| Grafana | http://localhost:3000 | Dashboard, Explore, dan korelasi telemetry |
| Loki | http://localhost:3100 | Backend penyimpanan logs |
| Tempo | http://localhost:3200 | Backend penyimpanan traces |
| OpenTelemetry Collector | `localhost:4317` / `localhost:4318` | Receiver OTLP gRPC / HTTP |

Grafana diprovisikan dengan data source Prometheus, Loki, dan Tempo. Login awal Grafana menggunakan `admin` / `admin` jika kredensial belum pernah diubah. Segera ganti password setelah login pertama.

Dashboard **DB Platform Observability** tersedia pada folder **DB Platform**. Dashboard berisi sembilan panel:

- Jobs per status.
- Queue wait p95.
- Migration duration p95.
- Target lock wait p95.
- Database pool connections.
- Pool acquisition wait p95.
- Retry count.
- Committed batches.
- Committed rows.

Dashboard dan provisioning data source berada di:

```text
deploy/observability/grafana/dashboards/dbplatform.json
deploy/observability/grafana/provisioning/dashboards/dashboards.yaml
deploy/observability/grafana/provisioning/datasources/datasources.yaml
```

### Metrics yang tersedia

Semua nama metric menggunakan prefix `dbplatform_`. Label dibatasi pada kumpulan nilai yang terkontrol; ID user, ID job, nama database, dan SQL tidak digunakan sebagai label metric.

| Metric | Tipe | Label | Keterangan |
|---|---|---|---|
| `dbplatform_jobs` | Gauge | `status` | Jumlah job untuk setiap status (`queued`, `running`, `retry_wait`, `succeeded`, `failed`, dan status terminal lain yang didukung). |
| `dbplatform_job_retries_total` | Counter | `reason` | Jumlah retry job. |
| `dbplatform_queue_wait_seconds` | Histogram | `mode` | Waktu tunggu job sebelum attempt dimulai, dipisahkan menurut mode standard/batch. |
| `dbplatform_lock_wait_seconds` | Histogram | `result` | Waktu tunggu PostgreSQL advisory lock. |
| `dbplatform_migration_duration_seconds` | Histogram | `action`, `result` | Durasi attempt migration. |
| `dbplatform_pool_connections` | Gauge | `state` | Jumlah koneksi aktif dan idle dari platform pool serta pool target yang dikelola. |
| `dbplatform_pool_wait_seconds` | Histogram | `result` | Waktu untuk memperoleh koneksi target. |
| `dbplatform_batches_committed_total` | Counter | `direction` | Jumlah batch yang memiliki commit proof, menurut arah `up`/`down`. |
| `dbplatform_rows_committed_total` | Counter | `direction` | Jumlah row yang memiliki commit proof, menurut arah `up`/`down`. |

Metrik job dan pool dikumpulkan secara periodik dengan interval lima detik. Prometheus melakukan scrape setiap 15 detik sesuai `deploy/observability/prometheus/prometheus.yml`.

Contoh query pada Prometheus → Graph:

```promql
# Jumlah job berdasarkan status
dbplatform_jobs

# Koneksi aktif dan idle
dbplatform_pool_connections

# Retry per menit dalam jendela lima menit
rate(dbplatform_job_retries_total[5m]) * 60

# p95 waktu tunggu antrean
histogram_quantile(0.95, sum by (le) (rate(dbplatform_queue_wait_seconds_bucket[5m])))

# p95 durasi migration
histogram_quantile(0.95, sum by (le) (rate(dbplatform_migration_duration_seconds_bucket[5m])))
```

### Logs dan traces

Middleware HTTP menerima `traceparent` dan `tracestate` sesuai konteks trace, serta menerima `X-Correlation-ID` yang tervalidasi. Jika correlation ID tidak diberikan, trace ID digunakan sebagai correlation ID. Response mengembalikan `traceparent` dan `X-Correlation-ID` untuk membantu penelusuran request.

Structured logs memuat event, timestamp, level, dan bila tersedia `trace_id`, `span_id`, serta `correlation_id`. Query PostgreSQL dicatat sebagai event dan span. Worker memulihkan trace context yang disimpan bersama job agar request awal dapat dikorelasikan dengan eksekusi worker.

Untuk melihat logs, buka Grafana → **Explore** → pilih data source **Loki**. Untuk melihat trace, pilih data source **Tempo**. Trace ID dari log dapat digunakan untuk mencari trace terkait. Collector mengirim logs ke Loki dan traces ke Tempo; metrics dikumpulkan langsung oleh Prometheus dari endpoint backend.

Informasi sensitif seperti password, token, secret, cookie, authorization, kredensial, dan parameter SQL disamarkan sebelum dicatat. Nilai parameter SQL tidak dicatat sebagai nilai asli. Telemetry exporter menggunakan antrean terbatas dan timeout agar pengiriman telemetry tidak menahan alur utama aplikasi; ketika antrean penuh atau Collector tidak tersedia, sebagian telemetry dapat terbuang tanpa menghentikan request aplikasi.

### Memeriksa apakah telemetry masuk

1. Pastikan semua container observability berstatus `Up` melalui `docker compose ps`.
2. Buka `http://localhost:8080/api/metrics`; endpoint harus menampilkan teks Prometheus dengan nama metric `dbplatform_`.
3. Buka Prometheus → **Status → Targets** dan pastikan target `dbplatform` berstatus **UP**.
4. Buka Grafana dan pastikan dashboard **DB Platform Observability** muncul pada folder **DB Platform**.
5. Buat aktivitas aplikasi, misalnya login, membuka halaman database, atau menjalankan job, lalu periksa logs di Loki dan traces di Tempo. Pengumpulan job metrics juga membutuhkan data job/attempt yang relevan.

### Menghentikan stack

```sh
docker compose down
```

Perintah tersebut menghentikan container tetapi mempertahankan volume. Volume `prometheus-data`, `loki-data`, `tempo-data`, dan `grafana-data` menyimpan data observability. Gunakan `docker compose down -v` hanya jika memang ingin menghapus seluruh volume, termasuk data database dan observability.
