# Ashanum Ecommerce

E-Commerce Go (Lightweight HG680P).

Storefront **Ashanum**: aplikasi e-commerce *modular monolith* dalam satu binary Go: server-side rendering
dengan `html/template` + HTMX, database PostgreSQL di server terpisah, berjalan di
belakang Nginx dan Cloudflare pada FiberHome HG680P (ARM64, RAM 2 GB).

Rancangan lengkap ada di [docs/blueprint.md](docs/blueprint.md). Pengembangan
dilakukan per fase.

| Fase | Cakupan | Status |
|------|---------|--------|
| 1 | Arsitektur, konfigurasi, PostgreSQL, migrasi, base template, server siap-Nginx | Selesai |
| 2 | Authentication, user, profil, alamat | Selesai |
| 3 | Kategori, produk, gambar, atribut, varian, storefront, pencarian | Selesai |
| 4 | Cart, cart item, tambah/ubah/hapus, ringkasan, persiapan Buy Now | Selesai |
| 5 | Checkout, order, snapshot alamat/produk, Beli Sekarang, riwayat & detail pesanan (shipping aggregator ditunda) | Selesai |
| 6 | Pembayaran Midtrans Snap (**sandbox saja**), webhook, kedaluwarsa & pembatalan pesanan, pengembalian stok, reconcile | Selesai |
| 7 | Ulasan & rating, pengajuan pengembalian (refund/tukar barang), bukti (URL), status retur, fondasi domain refund | Selesai |
| 8 | Admin (role & otorisasi), dashboard, produk/kategori/varian/stok, fulfillment pesanan, retur & refund (administratif), moderasi ulasan, laporan, audit log | Selesai |
| 9 | REST API v1 untuk mobile: access + refresh token (rotasi), akun, alamat, katalog, keranjang, checkout, pesanan, pembayaran, ulasan, retur, dokumentasi | Selesai |
| 10 | Pengiriman: provider abstraction + Biteship (mode testing), area alamat, berat produk, quote ongkir di checkout, snapshot pesanan, booking idempoten, tracking, webhook, reconcile-shipments | Selesai |
| 11+ | Security review, load testing, deployment HG680P, Midtrans produksi (belum dijadwalkan) | Belum |

## Stack

- Go 1.26, [chi](https://github.com/go-chi/chi) (router), [pgx v5](https://github.com/jackc/pgx) (driver + pool), sqlc (query bertipe)
- `html/template`, HTMX 2.0.11 (disimpan lokal di `web/static/js`), CSS biasa
- Dependency langsung: `chi`, `pgx`, dan `golang.org/x/crypto` (bcrypt).

## Menjalankan di lokal

Prasyarat: Go 1.26+, akses ke database PostgreSQL.

```bash
cp .env.example .env          # lalu isi DATABASE_URL (dan SESSION_SECRET)
go run ./cmd/web migrate      # terapkan migrasi
go run ./cmd/web              # buka http://127.0.0.1:8080
```

Pada `APP_ENV=development`, template dan file statis dibaca langsung dari folder
`web/`, jadi perubahan HTML/CSS cukup di-refresh tanpa restart. Pada `production`,
semuanya dibaca dari binary (embed).

### Perintah

| Perintah | Keterangan |
|----------|------------|
| `ecommerce` / `ecommerce serve` | Jalankan web server |
| `ecommerce migrate` | Terapkan migrasi yang belum dijalankan |
| `ecommerce migrate status` | Daftar migrasi dan waktu diterapkan |
| `ecommerce create-admin -email E -first-name N [-last-name N] [-promote]` | Buat akun admin (password dibaca dari stdin, bukan flag); `-promote` menjadikan akun yang sudah ada admin |
| `ecommerce recalculate-ratings` | Perbaiki cache rating produk dari `product_reviews` (alat perbaikan) |
| `ecommerce reconcile-payments [-limit N]` | Sinkronkan pembayaran pending dengan Midtrans, batalkan pesanan lewat batas bayar (stok kembali), daftar pembayaran yang perlu ditinjau |
| `ecommerce cleanup-api-sessions` | Hapus sesi API (mobile) yang kedaluwarsa/dicabut; untuk cron/systemd timer |
| `ecommerce reconcile-shipments [-limit N]` | Sinkron status pengiriman dari penyedia, laporkan booking tanpa jawaban, hapus quote kedaluwarsa; untuk cron/systemd timer |
| `ecommerce version` | Versi build |

`Makefile` menyediakan pintasan (`make run`, `make test`, `make build-arm64`, ...).
Di Windows tanpa `make`, jalankan perintah `go` yang ada di dalam Makefile secara langsung.

Tanpa `SESSION_SECRET`, mode development memakai secret acak per proses (sesi hilang
saat restart). Di production `SESSION_SECRET` wajib, minimal 32 karakter, misalnya hasil
`openssl rand -base64 48`.

### Halaman (Fase 2)

| Route | Keterangan |
|-------|------------|
| `GET/POST /register`, `GET/POST /login` | Daftar & masuk (email + password), dibatasi rate limit per IP |
| `POST /logout` | Keluar (hapus sesi) |
| `GET /account` | Akun Saya: profil ringkas, pintasan pesanan, menu |
| `GET/POST /account/profile` | Profil (HTMX: simpan tanpa reload) |
| `GET /account/addresses` | Alamat Saya |
| `GET /account/addresses/new`, `POST /account/addresses` | Tambah alamat |
| `GET /account/addresses/{id}/edit`, `POST /account/addresses/{id}` | Ubah alamat |
| `POST /account/addresses/{id}/primary` | Jadikan utama (HTMX) |
| `POST /account/addresses/{id}/delete` | Hapus (HTMX + konfirmasi) |

Semua `/account/*` mewajibkan login; tamu diarahkan ke `/login?next=...`.

### Halaman storefront (Fase 3)

| Route | Keterangan |
|-------|------------|
| `GET /` | Home: promo, kategori, Flash Sale, Terlaris, Rekomendasi Untukmu |
| `GET /categories` | Semua kategori aktif (tab "Kategori" di mobile) |
| `GET /categories/{slug}` | Produk per kategori, dengan filter harga dan sort |
| `GET /search?q=&category=&min=&max=&flash=1&sort=&page=` | Pencarian dan daftar semua produk |
| `GET /products/{slug}?o=<id nilai>,…` | Detail produk; pilihan varian lewat `?o=` (HTMX menukar panel harga) |

Sort: `relevance` (default jika ada `q`), `newest`, `bestselling`, `price_asc`,
`price_desc`. Paginasi 20 produk per halaman, tombol "Muat lebih banyak" memakai HTMX
dan tetap berupa link biasa tanpa JavaScript. Keranjang, Beli Sekarang, dan Pesanan masih
placeholder nonaktif (`data-phase="4"`/`"5"`).

### Keranjang (Fase 4)

| Route | Keterangan |
|-------|------------|
| `GET /cart` | Keranjang; pilihan item lewat `?sel=1&item=<id>…` (HTMX: hanya bagian keranjang) |
| `POST /cart/items` | Tambah varian (`variant_id`, `quantity`, `return_to`) |
| `POST /cart/items/{id}` | Ubah jumlah (`quantity`) |
| `POST /cart/items/{id}/delete` | Hapus item |
| `POST /cart/clear` | Kosongkan keranjang |

Semua route keranjang wajib login. Tamu yang menekan "Masukkan Keranjang" diarahkan ke
login lalu kembali ke produk dengan varian dan jumlah yang sama.

### Checkout & pesanan (Fase 5)

| Route | Keterangan |
|-------|------------|
| `GET /checkout?item=<id>&item=…` | Checkout item keranjang yang dipilih |
| `GET /checkout?variant_id=…&quantity=…` | Beli Sekarang (tanpa mengubah keranjang) |
| `GET /checkout?…&address=<id>` | Ganti alamat (HTMX: hanya bagian checkout) |
| `POST /checkout/order` | Buat pesanan (satu transaksi), lalu ke halaman sukses |
| `GET /checkout/success/{nomor}` | Pesanan berhasil dibuat |
| `GET /account/orders?status=unpaid\|processing\|shipped\|completed\|cancelled` | Pesanan Saya |
| `GET /account/orders/{nomor}` | Rincian pesanan |

Semua wajib login; pesanan dan item keranjang hanya bisa diakses pemiliknya.
Sejak Fase 10 checkout menampilkan pilihan kurir dan total sudah termasuk ongkir (lihat
Pengiriman); voucher masih "belum tersedia"; pesanan dibuat berstatus Belum Bayar.

### Pembayaran (Fase 6)

> **Sandbox saja.** `MIDTRANS_ENV` selain `sandbox` ditolak saat startup. Sejak Fase 10
> `orders.total` sudah termasuk ongkos kirim (baris "Ongkos Kirim" di `item_details`), tetapi
> pembayaran produksi baru boleh diaktifkan setelah Midtrans Sandbox asli dan Biteship
> diuji end-to-end serta review pembayaran produksi selesai.

| Route | Keterangan |
|-------|------------|
| `POST /account/orders/{nomor}/pay` | Bayar Sekarang: buat/pakai ulang attempt, 303 ke halaman Midtrans Snap |
| `POST /account/orders/{nomor}/cancel` | Batalkan pesanan belum bayar (stok dikembalikan) |
| `GET /account/orders/{nomor}/payment/finish` | Kembali dari Midtrans: cek status ke Midtrans, tampilkan hasil |
| `POST /webhooks/midtrans` | HTTP Notification Midtrans (publik, tanpa sesi, wajib signature) |

Pengaturan Midtrans Sandbox (dashboard sandbox → Settings → Payment):

- **Notification URL**: `https://<domain>/webhooks/midtrans`
- **Finish Redirect URL** tidak wajib: aplikasi mengirim `callbacks.finish` sendiri
  (`APP_BASE_URL` + `/account/orders/{nomor}/payment/finish`).
- `.env`: `MIDTRANS_ENV=sandbox`, `MIDTRANS_SERVER_KEY=SB-Mid-server-…`, `APP_BASE_URL`.
  Tanpa server key, tombol bayar tampil "Pembayaran online belum tersedia".

Di development, Midtrans tidak bisa mengirim notifikasi ke `127.0.0.1`; halaman finish
dan `reconcile-payments` menanyakan status langsung ke Midtrans sehingga alur tetap jalan.

`reconcile-payments` tidak menyimpan state dan aman dijalankan berulang. Belum ada
scheduler terpasang; nanti bisa dipanggil systemd timer (mis. tiap 15 menit) dengan
`EnvironmentFile` yang sama seperti service.

### Ulasan & pengembalian (Fase 7)

| Route | Keterangan |
|-------|------------|
| `GET /products/{slug}/reviews?page=N` | Semua ulasan produk (publik; HTMX "muat lagi") |
| `GET /account/reviews?tab=pending` | Ulasan Saya / Belum Diulas |
| `GET /account/reviews/new?item=<order_item_id>` · `POST /account/reviews` | Beri ulasan |
| `GET /account/reviews/{id}/edit` · `POST /account/reviews/{id}` | Ubah ulasan sendiri |
| `GET /account/returns` | Riwayat pengembalian |
| `GET /account/returns/new?order=<nomor>` · `POST /account/returns` | Ajukan pengembalian dana / tukar barang |
| `GET /account/returns/{nomor}` | Rincian & timeline pengembalian |
| `POST /account/returns/{nomor}/cancel` | Batalkan pengajuan yang masih menunggu persetujuan |

Ulasan dan pengembalian hanya untuk pesanan **COMPLETED** (pengembalian juga wajib sudah
dibayar). Sejak Fase 8 pesanan mencapai COMPLETED lewat alur admin (lihat di bawah);
langkah retur setelah pengajuan juga dikerjakan admin.

Batas pengajuan pengembalian: `RETURN_WINDOW_DAYS` (default 7) hari setelah
`orders.completed_at` — kebijakan bisnis yang bisa diubah, bukan aturan tetap.

### Pengiriman (Fase 10)

Detail: [docs/shipping.md](docs/shipping.md). Ongkir dihitung dari penyedia (Biteship,
**mode testing**; `BITESHIP_ENV=live` ditolak kecuali `SHIPPING_LIVE_ENABLED=true`),
dipilih pembeli saat checkout, dan masuk ke `orders.total` yang ditagih Midtrans.

- Alamat menyimpan area pengiriman dari pencarian area (`shipping_area_id`, netral
  penyedia). Produk aktif wajib punya berat (gram); varian boleh menimpa.
- Checkout menampilkan opsi kurir sebagai *quote* yang disimpan server (kedaluwarsa,
  terikat pembeli/alamat/paket); pesanan hanya menerima id quote → snapshot pengiriman.
  Penyedia mati → "Pengiriman sementara tidak dapat dihitung" (tidak pernah ongkir 0).
- Admin: "Buat Pengiriman" untuk pesanan dibayar & diproses (idempoten, aman diklik ulang),
  "Perbarui Status", "Batalkan pengiriman", dan kirim manual sebagai cadangan.
- Tracking dari `POST /webhooks/shipping/biteship` (header rahasia), admin, atau
  `reconcile-shipments`; halaman pesanan dan `GET /api/v1/orders/{n}/tracking` membaca
  database saja.

| Variabel | Default | Keterangan |
|---|---|---|
| `BITESHIP_API_KEY` | kosong | Kosong = ongkir tidak bisa dihitung, pesanan tidak bisa dibuat |
| `BITESHIP_ENV` / `SHIPPING_LIVE_ENABLED` | `test` / `false` | Live hanya bila keduanya diset |
| `BITESHIP_WEBHOOK_HEADER` / `BITESHIP_WEBHOOK_SECRET` | `X-Ashanum-Webhook-Secret` / kosong | Autentikasi webhook |
| `SHIP_FROM_*` | kosong | Alamat toko (nama, telepon, alamat, kode pos; area & koordinat opsional) |
| `SHIPPING_COURIERS` | `jne,sicepat,jnt,anteraja,pos` | Kurir yang ditawarkan |
| `SHIPPING_RATE_CACHE_TTL` / `SHIPPING_QUOTE_TTL` | `10m` / `30m` | Cache tarif / umur quote |

### REST API untuk mobile (Fase 9)

Semua di `/api/v1`, JSON, memakai service yang sama dengan web. Dokumentasi lengkap:
[docs/api-v1.md](docs/api-v1.md), kontrak mesin [docs/openapi.yaml](docs/openapi.yaml)
(dokumen saja, tanpa server Swagger), alur aplikasi [docs/mobile-flow.md](docs/mobile-flow.md).

- Auth: `POST /auth/register|login|refresh|logout`, `/auth/logout-all`; Bearer access
  token (default 15 menit) + refresh token berotasi (default 30 hari) dengan deteksi reuse.
- Akun: `/me`, `/me/profile`, `/me/addresses`. Katalog publik: `/categories`, `/home`,
  `/products` (cari/filter/sort/paginasi), `/products/{slug}`, `/products/{slug}/reviews`.
- Belanja: `/cart`, `/checkout/preview`, `/orders` (idempotent), `/orders/{nomor}`
  (`/cancel`, `/payment`). Purna jual: `/me/reviews`, `/reviews`, `/me/returns`, `/returns`.

| Variabel | Default | Keterangan |
|---|---|---|
| `API_ACCESS_TOKEN_TTL` | `15m` | Umur access token (1m–24h) |
| `API_REFRESH_TOKEN_TTL` | `30d` | Umur refresh token, bergeser tiap rotasi (1h–365d) |
| `API_CORS_ALLOWED_ORIGINS` | kosong | Origin browser yang boleh (persis, dipisah koma; `*` ditolak). Kosong = tertutup |
| `API_PAYMENT_FINISH_URL` | kosong | App link tujuan kembali dari Midtrans (`{order_number}` diganti). Kosong = halaman finish web |

### Admin (Fase 8)

Tidak ada admin bawaan atau password default. Buat admin pertama di server:

```bash
ecommerce create-admin -email admin@toko.id -first-name Rina      # password diketik 2x (tanpa echo)
printf '%s\n' "$PASSWORD" | ecommerce create-admin -email admin@toko.id -first-name Rina
ecommerce create-admin -email budi@example.com -promote             # akun yang sudah ada → ADMIN
```

Password admin minimal 12 karakter, disimpan sebagai bcrypt, tidak pernah dicetak/di-log.
Email yang sudah terdaftar **tidak** dinaikkan diam-diam: harus memakai `-promote`
(password akun itu tidak berubah). Setiap pemberian peran tercatat di audit log.

Semua `/admin/*`: wajib login **dan** role ADMIN (dibaca dari database pada setiap
request, jadi mencabut role langsung berlaku). Tamu diarahkan ke `/login`; akun lain
mendapat 404 seperti halaman yang tidak ada. Mutasi hanya POST dan melewati cek
same-origin (CSRF). Respons `Cache-Control: private, no-store` dan `X-Robots-Tag: noindex`.

| Route | Keterangan |
|-------|------------|
| `GET /admin` | Dashboard operasional (angka langsung dari DB) |
| `GET /admin/orders?status=&q=&page=` · `GET /admin/orders/{nomor}` | Daftar (cari nomor pesanan) & rincian |
| `POST /admin/orders/{nomor}/confirm` · `/ship` · `/complete` · `/cancel` | WAITING_CONFIRMATION → PROCESSING → SHIPPED → COMPLETED; batal (`/ship` = kirim manual) |
| `POST /admin/orders/{nomor}/shipment/book` · `/shipment/sync` · `/shipment/cancel` | Buat pengiriman di penyedia (idempoten), perbarui status, batalkan sebelum dijemput |
| `GET /admin/products` · `/products/new` · `POST /admin/products` | Daftar (cari nama/SKU), tambah produk (varian dibuat dari kombinasi atribut) |
| `GET /admin/products/{id}/edit` · `POST /admin/products/{id}` | Ubah info & gambar |
| `POST /admin/products/{id}/activate` · `/deactivate` | Tampil/sembunyikan (tidak ada hapus) |
| `POST /admin/products/{id}/variants` · `/variants/{vid}` · `/attributes` | Tambah/ubah varian (SKU, harga, aktif), tambah atribut |
| `GET /admin/stock?status=low|out` · `GET|POST /admin/stock/{vid}` | Daftar stok & penyesuaian stok beralasan + riwayat |
| `GET|POST /admin/categories` · `/categories/{id}/edit` · `POST /admin/categories/{id}` | Kategori (nonaktifkan, tidak dihapus) |
| `GET /admin/returns` · `GET /admin/returns/{nomor}` | Daftar & rincian retur |
| `POST /admin/returns/{nomor}/approve` · `/reject` · `/waiting-return` · `/received` · `/restock` | Setujui (boleh sebagian) / tolak, menunggu barang, terima & periksa, kembalikan ke stok |
| `POST /admin/returns/{nomor}/prepare-refund` · `/complete-refund` · `/complete-exchange` · `/complete` · `/cancel` | Refund & tukar barang, selesai, batal |
| `GET /admin/returns/{nomor}/estimate` | Estimasi refund saat jumlah disetujui diubah (fragment HTMX) |
| `GET /admin/refunds` · `/refunds/{id}` · `POST …/processing` · `…/complete` · `…/fail` | Administrasi refund (manual) |
| `GET /admin/reviews` · `POST /admin/reviews/{id}/hide` · `/unhide` | Moderasi ulasan (alasan wajib saat menyembunyikan) |
| `GET /admin/reports?range=today|7d|30d|custom&from=&to=` · `GET /admin/audit` | Laporan dasar & log audit |

Daftar admin: 20 per halaman (`?per=` maks. 100), pencarian memakai parameter SQL dan
`ILIKE` yang di-escape. HTMX hanya untuk filter/tab/paginasi, estimasi persetujuan, dan
muat ulang dashboard; semua aksi tetap berupa form biasa yang berjalan tanpa JavaScript.

`LOW_STOCK_THRESHOLD` (default 5): varian dengan sisa 1..N unit dihitung "stok rendah" di
dashboard/laporan. Tidak ada notifikasi atau worker.

### Data demo (development)

```bash
go run ./scripts/seeddemo     # memakai DATABASE_URL dari .env
```

Mengisi database development yang **kosong** dengan 9 kategori dan 26 produk demo
(atribut, varian, stok, ilustrasi SVG). Menolak berjalan jika `APP_ENV=production` atau
jika sudah ada produk. Tool ini tidak ikut masuk binary produksi.

### Test

```bash
go test ./...                                   # unit test; test database di-skip
TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/ecommerce_test?sslmode=disable go test ./...
# opsional, butuh internet + server key sandbox (variabel terpisah dari konfigurasi app):
MIDTRANS_SANDBOX_SERVER_KEY=SB-Mid-server-... go test -run Sandbox -v ./internal/payment/midtrans/
```

Test pembayaran memakai provider tiruan dan server Midtrans tiruan di memori
(`internal/payment/midtrans/midtranstest`), tanpa internet.

Dengan `TEST_DATABASE_URL`, setiap test membuat schema PostgreSQL sendiri (`test_xxxx`),
menjalankan migrasi, lalu menghapusnya. Gunakan database khusus test, jangan database produksi.

### Endpoint operasional

- `GET /healthz`: liveness, selalu `200 ok` selama proses hidup.
- `GET /readyz`: readiness, `200` jika database merespons dalam 2 detik, `503` jika tidak. Di Nginx hanya bisa diakses dari localhost.

## Struktur

```text
cmd/web/main.go              entry point + subcommand (serve, migrate, version)
internal/config/             konfigurasi dari environment (+ .env saat development)
internal/database/           pool pgx, migration runner, migrations/, queries/
internal/middleware/         logging, recovery, security headers + real IP, CSRF, rate limit
internal/view/               renderer template + static asset (cache-busting)
internal/server/             router, health check, http.Server + graceful shutdown
internal/home/               halaman beranda
internal/auth/               register, login, logout, sesi, middleware RequireUser
internal/user/               Akun Saya + profil
internal/address/            Alamat Saya (satu alamat utama per user)
internal/category/           kategori + cache memori daftar kategori aktif
internal/product/            katalog: listing, pencarian, detail, varian, home sections
internal/media/              aturan URL gambar (Cloudinary/data URI) + resize Cloudinary
internal/cart/               keranjang: satu cart aktif per user, item per varian
internal/checkout/           checkout & pembuatan pesanan (transaksi, stok, snapshot)
internal/order/              Pesanan Saya: riwayat & rincian pesanan, halaman hasil pembayaran
internal/payment/            pembayaran: attempt, webhook, status, batal/kedaluwarsa, stok kembali, reconcile
internal/payment/midtrans/   klien Midtrans (Snap redirect + Core API status) dengan net/http, sandbox saja
internal/review/             ulasan & rating produk (sumber kebenaran product_reviews)
internal/returns/            pengajuan pengembalian, persetujuan sebagian, inspeksi, restock, tukar barang, refund
internal/admin/              HTTP back office /admin (tanpa aturan bisnis; memanggil service)
internal/fulfillment/        alur pesanan admin: konfirmasi, kirim (manual), selesai, batal + refund
internal/inventory/          penyesuaian stok + jejak inventory_adjustments
internal/report/             dashboard & laporan (SQL agregat)
internal/audit/              admin audit log (append-only)
internal/api/                REST API /api/v1 (JSON, Bearer token) di atas service yang sama
internal/shipping/           ongkir, quote, booking, tracking, webhook (provider-neutral)
internal/shipping/biteship/  adapter Biteship (net/http) + biteshiptest (tiruan untuk tes)
internal/validate/           aturan validasi input bersama (email, HP, nama, password)
internal/database/dbgen/     kode hasil sqlc (jangan diedit manual)
internal/testutil/           schema PostgreSQL terisolasi untuk test
web/templates/               layouts/, partials/, <modul>/, errors/
web/static/                  css/, js/, icons/, img/ (placeholder)
scripts/seeddemo/            pengisi data demo untuk development
docs/                        blueprint, rencana kode wilayah alamat, api-v1.md, openapi.yaml, mobile-flow.md, shipping.md
deploy/nginx/                konfigurasi Nginx + real IP Cloudflare
deploy/systemd/              unit systemd
```

Perbedaan dari struktur di blueprint:

- `migrations/` dan `sql/` di root tidak dibuat. Migrasi hanya ada di
  `internal/database/migrations/` (di-embed ke binary) dan menjadi sumber schema untuk
  sqlc; query di `internal/database/queries/`. Satu sumber, tidak ada duplikasi.
- Tambahan `internal/view/` dan `internal/server/` untuk rendering dan wiring router,
  serta `deploy/` untuk konfigurasi Nginx/systemd.
- Middleware sesi (`LoadSession`, `RequireUser`) ada di `internal/auth/middleware.go`,
  bukan `internal/middleware/auth.go`: middleware itu butuh auth service, dan auth memakai
  paket `middleware` (rate limit), jadi menaruhnya di sana akan membuat import cycle.
- Alamat dipisah ke modul `internal/address/` (blueprint menaruhnya di modul user) agar
  service-nya bisa dipakai ulang langsung oleh API `/api/v1/addresses`.
- Layout akun bernama `layouts/account.html` (bukan `user.html`), template halaman akun di
  `web/templates/user/`. Layout admin: `layouts/admin.html` + `web/static/css/admin.css`
  (dimuat hanya di area admin).
- Admin blueprint `internal/admin/{dashboard,product,…}` disederhanakan: satu paket HTTP
  `internal/admin/` di atas service domain. Aturan bisnis tetap di modul domainnya
  (`fulfillment`, `inventory`, `product`, `category`, `returns`, `review`, `report`), sehingga
  API admin kelak memakai service yang sama. Middleware admin (`RequireAdmin`) ada di
  `internal/auth/middleware.go`, dengan alasan yang sama seperti `RequireUser`.
- Tidak ada partial `pagination`: listing memakai "Muat lebih banyak" (fragment
  `result-items` di `product/listing.html`).
- Halaman `/categories/{slug}` ditangani modul `product` (isinya daftar produk); modul
  `category` menangani `/categories` dan data kategori.
- Field produk `Price`, `OriginalPrice`, `Stock` tidak disimpan langsung di produk:
  sumbernya varian. `DiscountPercentage` dihitung dari harga, tidak disimpan. `BestSeller`
  tidak berupa flag manual: section Terlaris diurutkan dari `sold_count`.

## Konvensi

**Modul.** Setiap modul di `internal/<modul>/` mengikuti alur
`Handler → Service → Repository → PostgreSQL`, dan mendaftarkan route lewat method
`Routes(r chi.Router)` yang dipanggil di `internal/server/server.go`.

**Migrasi.** File `internal/database/migrations/NNNN_nama.sql`, hanya maju
(forward-only), tiap file dijalankan dalam satu transaksi dan dicatat di tabel
`schema_migrations` beserta checksum. Migrasi yang sudah diterapkan **tidak boleh
diedit**: runner akan menolak. Buat migrasi baru untuk perubahan.
Konvensi kolom: `id bigint GENERATED ALWAYS AS IDENTITY`, `created_at`/`updated_at
timestamptz`, uang dalam `bigint` rupiah, dan trigger `set_updated_at()`.

**sqlc.** Query ditulis di `internal/database/queries/<modul>.sql`, lalu
`sqlc generate` (sqlc v1.31.1) menghasilkan package `internal/database/dbgen`, yang
di-commit. Repository tiap modul membungkus `dbgen` dan memetakan ke tipe domain, jadi
service tidak bergantung pada kode hasil generate. Pasang sqlc dari
[rilis resmi](https://github.com/sqlc-dev/sqlc/releases) atau lewat Docker:
`docker run --rm -v "${PWD}:/src" -w /src sqlc/sqlc generate`.

**Service tanpa HTTP.** Service (`auth`, `user`, `address`) hanya menerima `context`
dan parameter biasa, lalu mengembalikan tipe domain atau error (`validate.Errors` untuk
input tidak valid). Cookie, form, HTMX, dan template hanya ada di handler/middleware,
sehingga handler JSON `/api/v1/...` nanti cukup memanggil service yang sama.
Web memakai session cookie server-side; mobile/API boleh memakai transport autentikasi
sendiri (API token atau mobile session, diputuskan di fase mobile API) yang tetap
memanggil `auth.Service` yang sama. Token sesi browser tidak harus dipakai sebagai
Bearer token mobile.

**Sesi.** Cookie `__Host-sid` (production) / `sid` (development): HttpOnly, Secure
(production), SameSite=Lax, berisi token acak 256-bit. Database hanya menyimpan
HMAC-SHA256(`SESSION_SECRET`, token). Masa berlaku 30 hari dan diperpanjang otomatis jika
sisa kurang dari 15 hari. Mengganti `SESSION_SECRET` mengeluarkan semua user.

**Katalog.** Harga dan stok hanya ada di `product_variants`; produk tanpa opsi tetap
punya satu varian. Kolom `products.price`, `price_max`, `original_price`, `in_stock`
adalah ringkasan varian aktif yang dirawat trigger database (untuk filter/sort cepat),
jangan ditulis dari aplikasi. Composite foreign key di `product_variant_values` menjamin
varian, atribut, dan nilai berasal dari produk yang sama. `sold_count` dan `rating_*`
nanti dirawat modul pesanan dan ulasan.

**Keranjang.** Item merujuk `variant_id` (unik per cart; menambah varian yang sama
menaikkan jumlah lewat satu UPSERT atomik). Keranjang **bukan** sumber harga atau stok:
harga, stok, dan ketersediaan dibaca langsung dari katalog setiap kali ditampilkan, stok
tidak dipesan/dikurangi, dan checkout (Phase 5) wajib memvalidasi ulang. Jumlah per item
1–99 (`CHECK` di database), maksimal 50 item berbeda per cart. Penambahan yang melebihi
stok **ditolak** dengan pesan, tidak dipotong diam-diam. Item yang produknya nonaktif,
dihapus, atau habis tetap tampil sebagai tidak tersedia dan hanya bisa dihapus. Pilihan
item untuk checkout adalah state request (checkbox), tidak disimpan. Badge keranjang =
total quantity seluruh item (2 × A + 3 × B = 5), ditampilkan "99+" di atas 99.
`cart.Service.Quote(variant, qty)` memvalidasi pembelian tanpa menyimpan apa pun —
titik masuk "Beli Sekarang" di Phase 5.

**Checkout & pesanan.** Server satu-satunya sumber kebenaran: browser hanya mengirim
ID (item keranjang, varian, alamat), jumlah, total yang dilihat pengguna (untuk
mendeteksi perubahan harga), dan kunci idempotensi. `checkout.Service.PlaceOrder` dalam
**satu transaksi**: mengunci baris keranjang terpilih, mengunci varian dengan urutan id
(mencegah deadlock), memvalidasi ulang ketersediaan/stok/jumlah dengan data terbaru,
membandingkan total, lalu satu statement menulis `orders` + `order_items`, mengurangi
stok dengan guard `stock >= qty`, dan menghapus hanya item keranjang yang dibeli. Gagal di
titik mana pun → rollback total. Pembeli bersamaan untuk stok terakhir: hanya satu yang
berhasil, sisanya mendapat "Stok produk berubah…". Kirim ulang form dengan kunci yang sama
mengembalikan pesanan yang sama. Order menyimpan **snapshot** alamat dan setiap item
(nama, SKU, varian, gambar, harga satuan) sehingga riwayat tidak berubah saat katalog atau
alamat diubah/dihapus. Nomor pesanan `ASH-YYYYMMDD-NNNNNN` (tanggal WIB + sequence
PostgreSQL; unik dan aman konkurensi, bukan primary key). Stok dikurangi saat pesanan
dibuat dan dikembalikan saat pesanan belum bayar dibatalkan atau lewat batas bayar (lihat
Pembayaran). Ongkir dan pilihan kurir disalin dari quote ke snapshot pesanan (Fase 10,
lihat [docs/shipping.md](docs/shipping.md)); pengirimannya ada di tabel `shipments`.

**Pembayaran.** Midtrans Snap mode **redirect**: "Bayar Sekarang" → 303 ke halaman
pembayaran Midtrans, jadi tidak ada script Midtrans di halaman kita dan client key tidak
dipakai. Satu pesanan boleh punya beberapa *attempt* (`payments`), tetapi paling banyak
satu `PENDING` dan satu `PAID` (partial unique index); tiap attempt punya
`provider_order_id` sendiri `ASH-YYYYMMDD-NNNNNN-P<n>` yang tidak pernah dipakai ulang.
Nominal selalu `orders.total` (barang + ongkir − diskon; layanan pembayaran tidak menghitung ongkir). Klik ganda
memakai attempt pending yang sama (mutex per pesanan + constraint database). Sumber
kebenaran adalah Midtrans, bukan browser: webhook diterima hanya dengan signature
`SHA512(order_id + status_code + gross_amount + server key)` yang valid, lalu statusnya
**diambil ulang lewat Get Status API**; halaman finish juga hanya bertanya ke Midtrans.
Status Midtrans diterjemahkan di `internal/payment/midtrans` ke `PENDING | PAID | FAILED |
DENIED | CANCELLED | EXPIRED`; hanya attempt `PENDING` yang boleh berubah, status
terminal final. PAID → pesanan `WAITING_CONFIRMATION` (bukan `PROCESSING`), tanpa
mengubah stok. Nominal provider ≠ attempt ≠ `orders.total`, pembayaran setelah attempt
ditutup, atau refund/chargeback **tidak** diterapkan otomatis: ditandai `review_reason`
dan muncul di `reconcile-payments`. Semua transisi berjalan dalam transaksi yang
mengunci `orders` lebih dulu, lalu `payments`, lalu varian berurutan id (sama dengan
checkout); `payment_events` mencatat jejak audit (tanpa token/kunci).

**Batas bayar & stok.** `orders.payment_due_at` = waktu pesan + 24 jam; setiap attempt
kedaluwarsa pada waktu yang sama. Pesanan belum bayar yang dibatalkan pembeli atau lewat
batas bayar menjadi `CANCELLED` dan stoknya dikembalikan **sekali** (dijaga
`stock_restored_at`, idempoten untuk cancel ganda, webhook expire ganda, dan reconcile).
Kedaluwarsa dijalankan lazy saat halaman pesanan dibuka, oleh webhook expire, dan oleh
`reconcile-payments`; transaksi yang mungkin masih terbuka di Midtrans dicek/dibatalkan
di Midtrans dulu (tenggang 5 menit), dan jika Midtrans tidak bisa dihubungi pesanan
dibiarkan untuk percobaan berikutnya. Pembayaran gagal/ditolak/kedaluwarsa sebelum batas
bayar → tombol "Bayar Lagi" membuat attempt baru (maks. 10); pesanan yang sudah
dibatalkan tidak pernah dihidupkan lagi — pembeli membuat pesanan baru. Varian yang
dipegang pesanan belum bayar tidak bisa dihapus (trigger database); nonaktifkan saja.

**Ulasan & rating.** Satu ulasan per item pesanan (`UNIQUE(order_item_id)` + FK komposit
item→pesanan→user), hanya untuk pesanan COMPLETED milik sendiri. `product_reviews` adalah
sumber kebenaran; `products.rating_average/rating_count` hanya cache yang dihitung ulang
(AVG/COUNT) di **transaksi yang sama** dengan setiap ulasan baru atau diubah, setelah
baris produk dikunci — jadi dua ulasan bersamaan tidak saling menimpa agregat.
`ecommerce recalculate-ratings` memperbaiki cache jika perlu. Nama pengulas disamarkan
("S*** R********"); email, telepon, dan alamat tidak pernah tampil. Produk yang dihapus
tidak menghapus ulasan (`product_id` jadi NULL; riwayat memakai snapshot item pesanan).
Foto ulasan ditunda sampai ada uploader (rencana: tabel `review_images`, URL saja).

**Pengembalian.** Jenis `REFUND` (Pengembalian Dana) atau `EXCHANGE` (Tukar Barang),
alasan `DAMAGED | WRONG_ITEM | MISSING_ITEM | NOT_AS_DESCRIBED | VARIANT_ISSUE | OTHER`
(OTHER wajib deskripsi). Syarat: pesanan milik user, COMPLETED, punya payment PAID, dan
masih dalam batas waktu (jam server). Pengajuan dibuat dalam satu transaksi: kunci baris
pesanan → kunci item pesanan terpilih (urut id) → jumlah dicek terhadap retur lain yang
belum ditolak/dibatalkan → simpan retur, item, bukti. Dua pengajuan bersamaan tidak bisa
melebihi jumlah yang dibeli. Nomor `RET-YYYYMMDD-NNNNNN` dari sequence (bukan PK).
Nominal pengembalian dihitung server dari harga snapshot `order_items` × jumlah, hanya
nilai barang — ongkos kirim, ongkir retur, dan biaya lain **tidak** dihitung; kebijakannya
diputuskan setelah fase Shipping. Tukar barang tidak mengembalikan dana. Bukti berupa
**tautan https saja** (maks. 5, tanpa file lokal/IP/kredensial); hanya gambar Cloudinary
yang ditampilkan sebagai gambar (CSP), lainnya sebagai tautan. Uploader kelak cukup
menghasilkan URL, service retur tidak berubah.

**Status retur** (state machine, perubahan lain ditolak):
REFUND `REQUESTED → APPROVED → WAITING_RETURN → ITEM_RECEIVED → REFUND_PENDING →
REFUNDED → COMPLETED`; EXCHANGE `… → ITEM_RECEIVED → EXCHANGED → COMPLETED`;
`REQUESTED → REJECTED` (alasan wajib); `REQUESTED | APPROVED | WAITING_RETURN → CANCELLED`.
Pembeli hanya mengajukan dan boleh membatalkan selama REQUESTED; semua langkah lain
dikerjakan admin lewat `returns.Service` (state machine di `policy.go`, tidak diduplikasi
di handler). Setiap langkah tercatat di `return_events` (timeline) dan `admin_audit_logs`.

**Refund.** Tabel `refunds` (PENDING/APPROVED/PROCESSING/SUCCEEDED/FAILED/CANCELLED).
`PrepareRefund` membuat refund PENDING untuk payment PAID pesanan, dengan total refund
tidak boleh melebihi yang dibayar. **Tidak ada panggilan API refund Midtrans**; uang tidak
dikembalikan otomatis. `CompleteRefund` hanya mencatat refund yang dilakukan di luar
sistem (mis. transfer manual) beserta referensinya.

**Persetujuan sebagian.** `return_items.approved_quantity` (0..diminta, NULL sebelum
ditinjau) disimpan eksplisit; nilai refund = jumlah disetujui × harga yang dibayar
(dihitung server, tidak pernah dari form). Minimal satu unit harus disetujui; menyetujui
nol ditolak — gunakan Tolak (alasan wajib, tampil ke pembeli). Retur yang masih berlaku
menahan `coalesce(approved_quantity, quantity)` unit, jadi unit yang tidak disetujui bisa
diajukan lagi selama batas waktu.

**Inspeksi & restock.** Saat barang diterima admin wajib memilih kondisi tiap baris:
`RESTOCKABLE` / `DAMAGED` / `NON_RESTOCKABLE`. Menerima barang **tidak** mengubah stok.
"Kembalikan ke Stok" menambah stok hanya untuk baris RESTOCKABLE, sekali per baris
(`restocked_at` + `restocked_quantity`; permintaan ganda/bersamaan tidak menggandakan
stok), dengan adjustment `RETURN_RESTOCK`. Barang rusak tidak pernah kembali dijual.

**Tukar barang.** Admin memilih varian pengganti (varian aktif dari produk yang sama).
Stok pengganti baru dikurangi saat "Kirim Barang Pengganti", atomik
(`UPDATE … SET stock = stock - n WHERE id = … AND stock >= n`) dengan adjustment
`EXCHANGE_OUT`; stok kurang → langkah ditolak dan tidak ada yang berubah. Varian/SKU
pengganti disimpan di `return_items.replacement_*`.

**Refund (administratif).** Sumber `RETURN` atau `ORDER_CANCELLATION` (pesanan berbayar
yang dibatalkan admin). Alur: PENDING → PROCESSING → SUCCEEDED (referensi transfer wajib)
atau FAILED (alasan wajib; retur bisa disiapkan refund lagi). Total refund per payment
tidak bisa melebihi yang dibayar (baris payment dikunci). **Tidak ada panggilan API refund
Midtrans**: dana dikembalikan di luar sistem dan hasilnya dicatat.

**Fulfillment pesanan.** `WAITING_PAYMENT` hanya berpindah lewat pembayaran yang
dikonfirmasi Midtrans (Fase 6); admin tidak bisa menandai lunas, dan CHECK
`orders_paid_status_chk` menolak PROCESSING/SHIPPED/COMPLETED tanpa `paid_at`. Admin:
`WAITING_CONFIRMATION → PROCESSING` (konfirmasi) → `SHIPPED` (pengiriman dibooking di penyedia
dan dijemput kurir — Fase 10 — atau kurir, layanan, resi diisi manual sebagai cadangan) →
`COMPLETED` (`completed_at` di transaksi yang sama: ulasan & retur terbuka). Tidak ada
lompatan status. Pembatalan admin: pesanan belum bayar mengikuti aturan Fase 6 (transaksi
Midtrans dibatalkan dulu); pesanan berbayar yang belum dikirim → stok kembali sekali
(`stock_restored_at`, adjustment `ORDER_RESTORE`) + refund PENDING sebesar yang dibayar;
pesanan terkirim/selesai tidak bisa dibatalkan (pakai retur). Kunci: orders → payments →
variants (urut id).

**Stok & jejaknya.** `product_variants.stock` tetap satu-satunya angka stok.
`inventory_adjustments` (tipe `INITIAL_STOCK`, `MANUAL_ADJUSTMENT`, `RETURN_RESTOCK`,
`EXCHANGE_OUT`, `ORDER_RESTORE`; stok sebelum/sesudah, alasan, referensi, admin) ditulis
di transaksi yang sama dengan perubahan stok dan hanya berfungsi sebagai jejak. Penyesuaian
manual: kunci varian → validasi hasil ≥ 0 → update → adjustment → audit. Form produk/varian
tidak pernah menulis stok.

**Moderasi ulasan.** Admin bisa menyembunyikan (alasan wajib) dan menampilkan lagi; tidak
ada hapus. Ulasan tersembunyi tidak tampil publik dan **tidak dihitung** di rating —
rating dihitung ulang di transaksi yang sama. Pembeli tetap melihat (dan boleh mengubah)
ulasannya, dengan label "Tidak ditampilkan publik"; mengubahnya tidak membuatnya tampil.

**Laporan.** Penjualan = total pesanan yang **dibayar** pada periode (paid_at) dan tidak
dibatalkan; WAITING_PAYMENT dan keranjang tidak pernah dihitung; pesanan berbayar yang
dibatalkan ditampilkan terpisah. Hari dihitung dalam WIB. Semua angka dari SQL agregat
(satu query per blok, tanpa query per baris).

**Audit log.** `admin_audit_logs` (admin, aksi, entitas, label, metadata JSON) ditulis di
transaksi yang sama dengan aksinya; trigger menolak UPDATE/DELETE. Metadata berisi status,
jumlah, catatan — tidak pernah password, token, atau kunci.

**Sesi API (mobile).** Transport terpisah dari cookie web. Tabel `api_sessions`
menyimpan **HMAC** access token dan refresh token (256-bit acak, prefix `ashat_`/`ashrt_`),
bukan plaintext. Satu request API = satu query terindeks (token → sesi + user). Refresh
merotasi pasangan token dalam satu transaksi; refresh token yang sudah dirotasi dan dipakai
lagi mencabut seluruh family perangkat (`REFRESH_TOKEN_REUSED`). Logout API tidak menyentuh
sesi web dan sebaliknya. `LoadSession` web tidak berjalan di `/api/`, API tidak membaca
cookie, sehingga `/api/v1/` dikecualikan dari cek same-origin (CSRF) tanpa melemahkan web.
Rate limit login/register/refresh di memori (satu instance). Request JSON ketat: wajib
`application/json`, maks. 64 KiB, field tak dikenal ditolak; error memakai kontrak
`{"error":{code,message,fields}}` tanpa detail internal.

**Role.** `CUSTOMER` (default registrasi; role tidak pernah dibaca dari form) dan
`ADMIN`. Migrasi 0009 mengganti nama nilai lama `USER` menjadi `CUSTOMER`.

**Gambar.** Database menyimpan URL, bukan file. `media.Valid` hanya menerima URL https
Cloudinary atau data URI kecil (data demo); `{{img url lebar}}` meminta Cloudinary
me-resize, sehingga HG680P tidak memproses gambar. Host gambar lain harus ditambahkan di
`internal/media` dan di `img-src` CSP sekaligus.

**Pencarian.** Kolom `search_vector` (nama + merek, generated) dengan index GIN dan
konfigurasi `simple`; setiap kata dicocokkan sebagai prefiks dan semuanya harus cocok.
Nama kategori ikut dicocokkan. Tanpa Elasticsearch atau layanan eksternal.

**Template.** Halaman `web/templates/<modul>/<nama>.html` dirender dengan nama
`"<modul>/<nama>"`, memilih layout lewat `{{template "base" .}}` dan mengisi blok
`title`/`content`. Data dari handler tersedia di `.Data`, info aplikasi di `.App`.
Setiap halaman hanya memuat file layout/partial yang benar-benar dipanggilnya (closure
`{{template}}`, lihat `internal/view/deps.go`), bukan salinan semua file bersama: memori
template turun ±57% (7,2 → 3,1 MB) tanpa cache tambahan; template tetap embedded.
Untuk respons HTMX parsial gunakan `view.Fragment(...)` dengan nama blok `{{define}}`.

**Content Security Policy** hanya mengizinkan script/style dari domain sendiri, jadi:
tidak ada `<script>` inline, tidak ada atribut `style="..."`, dan tidak ada `hx-on`.
Ikon memakai sprite `web/static/icons/sprite.svg` lewat fungsi template `{{icon "nama"}}`.
Taruh CSS di `web/static/css/` dan JS di `web/static/js/`.

**CSRF.** Ditangani `http.CrossOriginProtection` (standard library): request POST/PUT/
PATCH/DELETE lintas origin dari browser ditolak `403`. Tidak perlu token di form.
Webhook server-ke-server (Midtrans) tetap lolos dan wajib memverifikasi signature.

**CSP & pembayaran.** Satu-satunya perubahan CSP Fase 6: `form-action` ditambah origin
halaman pembayaran (`https://app.sandbox.midtrans.com`), hanya jika server key diisi,
karena form "Bayar Sekarang" di-redirect ke sana. `script-src`, `connect-src`,
`frame-src` tidak diubah; tidak ada `unsafe-inline`, `unsafe-eval`, atau `*`.

## Deployment ke HG680P

Build di mesin development:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o ecommerce ./cmd/web
```

Hasilnya satu file ELF statis (~15 MB) yang sudah berisi template, CSS, JS, dan migrasi.

Persiapan sekali di perangkat (Armbian):

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ecommerce
sudo mkdir -p /opt/ecommerce /etc/ecommerce
# .env.production: salinan .env.example berisi nilai produksi (APP_ENV=production)
sudo install -m 600 -o root -g root .env.production /etc/ecommerce/ecommerce.env
sudo cp deploy/systemd/ecommerce.service /etc/systemd/system/
sudo cp deploy/nginx/cloudflare-realip.conf /etc/nginx/snippets/
sudo cp deploy/nginx/ecommerce.conf /etc/nginx/sites-available/
sudo ln -s /etc/nginx/sites-available/ecommerce.conf /etc/nginx/sites-enabled/
sudo systemctl daemon-reload && sudo systemctl enable ecommerce
```

Setiap rilis:

```bash
scp ecommerce user@hg680p:/tmp/
ssh user@hg680p '
  sudo install -m 755 /tmp/ecommerce /opt/ecommerce/ecommerce &&
  sudo systemd-run --quiet --wait --pipe --collect --uid=ecommerce --gid=ecommerce \
    -p EnvironmentFile=/etc/ecommerce/ecommerce.env /opt/ecommerce/ecommerce migrate &&
  sudo systemctl restart ecommerce'
```

`systemd-run` membaca file env persis seperti service (sebagai root), lalu menjalankan
migrasi sebagai user `ecommerce`.

Admin pertama (sekali, setelah rilis pertama; password diketik dua kali tanpa echo dan
tidak masuk riwayat shell):

```bash
ssh -t user@hg680p sudo systemd-run --quiet --wait --pty --collect --uid=ecommerce --gid=ecommerce \
  -p EnvironmentFile=/etc/ecommerce/ecommerce.env /opt/ecommerce/ecommerce create-admin -email admin@toko.id -first-name Rina
```

Catatan:

- Aplikasi mendengarkan di `127.0.0.1:8080`; hanya Nginx yang bisa mengaksesnya.
  Header `X-Real-IP` hanya dipercaya jika koneksi datang dari loopback.
- Cloudflare SSL/TLS mode **Full (strict)** dengan Cloudflare Origin CA certificate.
  Jika memakai Cloudflare Tunnel, lihat komentar di `deploy/nginx/ecommerce.conf`.
- `GOMEMLIMIT=256MiB` dan `MemoryMax=512M` di unit systemd membatasi memori aplikasi.
- Gunakan `sslmode=require` (atau `verify-full`) pada `DATABASE_URL` karena database ada
  di server lain.
