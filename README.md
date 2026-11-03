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
| 3 | Kategori, produk, gambar, atribut, varian | Belum |
| 4 | Cart | Belum |
| 5 | Checkout, order, shipping | Belum |
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
internal/validate/           aturan validasi input bersama (email, HP, nama, password)
internal/database/dbgen/     kode hasil sqlc (jangan diedit manual)
internal/testutil/           schema PostgreSQL terisolasi untuk test
web/templates/               layouts/, partials/, <modul>/, errors/
web/static/                  css/, js/, icons/
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
  `web/templates/user/`. Layout `admin.html` dan partial `product-card`, `pagination`
  dibuat di fase yang memakainya.

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
