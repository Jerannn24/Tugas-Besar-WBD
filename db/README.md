# PostgreSQL Lokal

PostgreSQL 16 menggunakan dua layanan terpisah: `postgres` untuk development dan `postgres-test` untuk test. Masing-masing memiliki database platform, target `a`/`b`, dan volume sendiri.

Jalankan semua perintah dari root repository.

## Setup

```sh
test -f .env || cp .env.example .env
```

Isi enam sandi bootstrap, owner, dan runtime di `.env` dengan nilai heksadesimal unik sepanjang 64 karakter, tanpa tanda kutip.

```sh
docker compose up -d postgres
docker compose --profile test up -d postgres-test
```

Koneksi antar-container memakai `postgres:5432` atau `postgres-test:5432`. Port database tidak dibuka ke host.

## Migrasi Platform

```sh
bash db/apply.sh test
bash db/apply.sh dev
```

Migrasi di `db/migrations/` menyiapkan identity, session, draft, dan history. Menjalankan ulang migrasi dengan checksum yang sama tidak mengubah data.

## Profil Akses Target

Isi tiga sandi executor, preparer, dan viewer untuk target yang dipilih, misalnya `DB_TEST_TARGET_A_*_PASSWORD`, dengan format sandi yang sama.

```sh
bash db/target-profile/apply.sh test a
bash db/target-profile/apply.sh dev a
```

Argumen yang tersedia: `test|dev` dan `a|b`. Profil menyiapkan schema `managed`, metadata internal `wbd_auth`, serta role executor, viewer, dan preparer. Profil menolak pengambilalihan schema atau role yang sudah ada.

## Catatan

- Simpan `.env` secara lokal; jangan masukkan ke Git.
- Jangan mengedit migrasi atau profil SQL yang sudah diterapkan; gunakan versi baru.
- Pertahankan volume Docker agar data tetap tersimpan. Mengubah `.env` tidak otomatis mengganti sandi database yang sudah dibuat.
- Integrasi koneksi dan eksekusi SQL melalui backend masih perlu disambungkan.
