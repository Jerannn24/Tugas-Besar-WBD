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
5. **Migrations**: buat draft migrasi, jalankan, dan lihat riwayat eksekusi.

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