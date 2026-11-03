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
| 6 | Midtrans, webhook, payment | Belum |
| 7 | Riwayat order, review, return, refund | Belum |
| 8 | Admin, dashboard, laporan, pengaturan | Belum |
| 9 | Security review, optimasi, load testing, deployment HG680P | Belum |

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
Pengiriman, voucher, dan pembayaran tampil sebagai "belum tersedia": ongkos kirim
"Belum dihitung", total belum termasuk ongkir, pesanan dibuat berstatus Belum Bayar.

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
```

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
internal/order/              Pesanan Saya: riwayat & rincian pesanan
internal/validate/           aturan validasi input bersama (email, HP, nama, password)
internal/database/dbgen/     kode hasil sqlc (jangan diedit manual)
internal/testutil/           schema PostgreSQL terisolasi untuk test
web/templates/               layouts/, partials/, <modul>/, errors/
web/static/                  css/, js/, icons/, img/ (placeholder)
scripts/seeddemo/            pengisi data demo untuk development
docs/                        blueprint, rencana kode wilayah alamat (Phase 5)
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
  `web/templates/user/`. Layout `admin.html` dibuat di fase admin.
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
dibuat dan **belum** dikembalikan jika pesanan tidak dibayar — pembatalan/kedaluwarsa
pembayaran dikerjakan di Phase 6. Pembayaran akan berupa tabel `payments` dan pengiriman
tabel `shipments` (kurir, layanan, ongkir, resi, estimasi) yang merujuk `orders.id`.

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
Untuk respons HTMX parsial gunakan `view.Fragment(...)` dengan nama blok `{{define}}`.

**Content Security Policy** hanya mengizinkan script/style dari domain sendiri, jadi:
tidak ada `<script>` inline, tidak ada atribut `style="..."`, dan tidak ada `hx-on`.
Ikon memakai sprite `web/static/icons/sprite.svg` lewat fungsi template `{{icon "nama"}}`.
Taruh CSS di `web/static/css/` dan JS di `web/static/js/`.

**CSRF.** Ditangani `http.CrossOriginProtection` (standard library): request POST/PUT/
PATCH/DELETE lintas origin dari browser ditolak `403`. Tidak perlu token di form.
Webhook server-ke-server (Midtrans) tetap lolos dan wajib memverifikasi signature.

## Deployment ke HG680P

Build di mesin development:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o ecommerce ./cmd/web
```

Hasilnya satu file ELF statis (~12 MB) yang sudah berisi template, CSS, JS, dan migrasi.

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

Catatan:

- Aplikasi mendengarkan di `127.0.0.1:8080`; hanya Nginx yang bisa mengaksesnya.
  Header `X-Real-IP` hanya dipercaya jika koneksi datang dari loopback.
- Cloudflare SSL/TLS mode **Full (strict)** dengan Cloudflare Origin CA certificate.
  Jika memakai Cloudflare Tunnel, lihat komentar di `deploy/nginx/ecommerce.conf`.
- `GOMEMLIMIT=256MiB` dan `MemoryMax=512M` di unit systemd membatasi memori aplikasi.
- Gunakan `sslmode=require` (atau `verify-full`) pada `DATABASE_URL` karena database ada
  di server lain.
