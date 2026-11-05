# Ashanum REST API v1

API untuk aplikasi mobile pembeli. API ini memakai **service yang sama** dengan web
(auth, user, address, category, product, cart, checkout, order, payment, review, returns);
tidak ada aturan bisnis kedua. Admin tetap server-rendered (tidak ada `/api/v1/admin`).

Kontrak mesin juga tersedia di [`openapi.yaml`](openapi.yaml) (hanya dokumen, tanpa
server Swagger). Alur aplikasi: [`mobile-flow.md`](mobile-flow.md).

## Dasar

| | |
|---|---|
| Base path | `https://<domain>/api/v1` |
| Format | JSON UTF-8. Mutasi wajib `Content-Type: application/json` |
| Batas body | 64 KiB (lebih besar → `413 PAYLOAD_TOO_LARGE`) |
| Field tak dikenal | ditolak (`400 UNKNOWN_FIELD`), agar salah ketik di aplikasi ketahuan |
| Uang | integer rupiah (`129000`), tanpa desimal; format "Rp" dilakukan klien |
| Waktu | RFC 3339 dengan zona WIB, mis. `2026-11-04T15:30:00+07:00` |
| ID | integer JSON (< 2^53). Pesanan & retur memakai nomor publik (`ASH-…`, `RET-…`) |
| Gambar | URL siap pakai: Cloudinary di-resize (thumbnail 200 px, kartu 400 px, detail 900 px) |

## Autentikasi

Web memakai cookie sesi; API memakai transport terpisah:

- **Access token** — opaque, 256-bit acak, prefix `ashat_`, umur `API_ACCESS_TOKEN_TTL`
  (default 15 menit). Dikirim sebagai `Authorization: Bearer <access_token>`.
- **Refresh token** — opaque, 256-bit acak, prefix `ashrt_`, umur `API_REFRESH_TOKEN_TTL`
  (default 30 hari), **sekali pakai** (rotasi).

Server hanya menyimpan HMAC-SHA256 token (dikunci `SESSION_SECRET`), tidak pernah
plaintext. Token hanya muncul di respons login/register/refresh dan tidak pernah di-log.
Password memakai bcrypt dan aturan yang sama dengan web; role tidak pernah dibaca dari
request (registrasi selalu `CUSTOMER`).

### Refresh & rotasi

`POST /auth/refresh {"refresh_token": "..."}` → pasangan token **baru**. Pasangan lama
langsung tidak berlaku (access token lama juga). Jika refresh token yang sudah dirotasi
dipakai lagi (dicuri atau di-replay), server menganggapnya **reuse**: seluruh "family"
sesi perangkat itu dicabut dan respons `401 REFRESH_TOKEN_REUSED` — pengguna harus login
lagi. Karena itu aplikasi harus menjalankan **satu refresh pada satu waktu** (antrekan
request selama refresh berjalan).

Saat access token kedaluwarsa, server menjawab `401 TOKEN_EXPIRED`: lakukan refresh lalu
ulangi request. `401 INVALID_TOKEN` (dicabut/logout/rotasi) berarti login ulang.

### Logout

- `POST /auth/logout` dengan `Authorization: Bearer` **atau** body `{"refresh_token": "..."}`
  (berguna bila access token sudah kedaluwarsa) → `204`. Mencabut sesi perangkat itu saja.
- `POST /auth/logout-all` (Bearer) → `204`. Mencabut semua sesi API pengguna (semua perangkat).

Logout API tidak menghapus sesi browser web, dan logout web tidak mencabut sesi mobile.

### Rate limit

Per IP klien, di memori proses (cukup untuk satu instance HG680P; jika kelak ada beberapa
instance, limit ini per instance):

| Endpoint | Batas |
|---|---|
| `POST /auth/register` | 10 / 10 menit |
| `POST /auth/login` | 20 / 5 menit |
| `POST /auth/refresh`, `POST /auth/logout` | 60 / 5 menit |
| `GET /shipping/areas`, `POST /shipping/rates` | 60 / menit (memanggil penyedia pengiriman) |

Lewat batas → `429 RATE_LIMITED` dengan header `Retry-After` (detik).

### CORS & CSRF

Aplikasi native tidak butuh CORS. Default **tertutup**: tanpa header CORS, preflight dari
origin lain → `403 CORS_FORBIDDEN`. Untuk klien browser (mis. tooling dev), isi
`API_CORS_ALLOWED_ORIGINS` dengan origin persis (dipisah koma); `*` ditolak saat start,
tanpa `Allow-Credentials` (API tidak memakai cookie).

API dikecualikan dari cek same-origin (CSRF) web karena kredensialnya header Bearer yang
tidak pernah dikirim browser secara otomatis; API juga tidak membaca cookie sama sekali.
Rute web tetap dilindungi seperti sebelumnya.

## Kontrak respons

Sukses, resource tunggal:

```json
{ "data": { "...": "..." } }
```

Sukses, daftar (paginasi berbasis halaman):

```json
{
  "data": [ { "...": "..." } ],
  "meta": { "page": 1, "per_page": 20, "total": 120, "has_more": true }
}
```

`page` mulai 1. `has_more` selalu ada. `total` hanya ada bila memang dihitung (daftar
produk); daftar lain (pesanan, ulasan, retur) memakai `has_more` saja agar tidak
menjalankan `COUNT` tambahan. `per_page` bisa diatur di `/products` (1–60, default 20);
daftar lain memakai ukuran tetap yang dilaporkan di `meta.per_page`. Alamat dan kategori
dikembalikan utuh (jumlahnya kecil dan terbatas), tanpa `meta`.

Error:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Periksa kembali data yang dikirim.",
    "fields": { "quantity": "Stok tersedia hanya 3." }
  }
}
```

`message` dan `fields` berbahasa Indonesia dan aman ditampilkan. Tidak pernah berisi error
PostgreSQL, stack trace, path paket, atau error mentah Midtrans.

| Status | `code` | Arti |
|---|---|---|
| 400 | `INVALID_REQUEST` | JSON rusak / tipe salah / parameter query tidak valid (lihat `fields`) |
| 400 | `UNKNOWN_FIELD` | Ada field yang tidak dikenal |
| 401 | `UNAUTHORIZED` | Tanpa `Authorization: Bearer` |
| 401 | `INVALID_TOKEN` | Token tidak dikenal, dicabut, atau sudah dirotasi |
| 401 | `TOKEN_EXPIRED` | Access token kedaluwarsa → refresh |
| 401 | `REFRESH_TOKEN_REUSED` | Refresh token lama dipakai lagi → sesi perangkat dicabut |
| 401 | `INVALID_CREDENTIALS` | Email/password salah |
| 403 | `CORS_FORBIDDEN` | Preflight dari origin yang tidak diizinkan |
| 404 | `NOT_FOUND` | Tidak ada, atau milik pengguna lain |
| 405 | `METHOD_NOT_ALLOWED` | |
| 409 | `EMAIL_TAKEN`, `ALREADY_REVIEWED`, `ALREADY_PAID` | Konflik dengan data yang ada |
| 409 | `STOCK_CHANGED`, `PRICE_CHANGED`, `PRODUCT_UNAVAILABLE` | Checkout: data berubah sejak preview |
| 409 | `SHIPPING_CHANGED` | Quote ongkir kedaluwarsa, sudah dipakai, milik orang lain, atau alamat/paket berubah → preview ulang |
| 409 | `ITEMS_NOT_SHIPPABLE` | Ada produk tanpa berat (penjual belum mengatur) |
| 409 | `REVIEW_NOT_ALLOWED`, `RETURN_NOT_ALLOWED`, `INVALID_STATE` | Status pesanan/retur tidak mengizinkan |
| 413 | `PAYLOAD_TOO_LARGE` | Body > 64 KiB |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Bukan `application/json` |
| 422 | `VALIDATION_ERROR` | Isian tidak valid (lihat `fields`) |
| 422 | `PRODUCT_UNAVAILABLE`, `CART_FULL`, `ADDRESS_LIMIT` | Keranjang/alamat |
| 422 | `SHIPPING_AREA_REQUIRED`, `NO_COURIER` | Alamat belum punya area pengiriman / tidak ada kurir untuk rute ini |
| 429 | `RATE_LIMITED` | Terlalu banyak percobaan (`Retry-After`) |
| 500 | `INTERNAL_ERROR` | Kesalahan server (detail hanya di log server) |
| 503 | `PAYMENT_UNAVAILABLE`, `PAYMENT_PROVIDER_UNAVAILABLE` | Pembayaran online belum aktif / Midtrans tidak terjangkau |
| 503 | `SHIPPING_PROVIDER_UNAVAILABLE` | Ongkir tidak dapat dihitung saat ini (penyedia tidak terjangkau/belum dikonfigurasi) |

Respons yang butuh login memakai `Cache-Control: no-store`.

## Endpoint

🔒 = wajib Bearer token.

### Akun

| Method | Path | Body / query | Sukses |
|---|---|---|---|
| POST | `/auth/register` | `{email, password, device_name?}` | 201 token + user |
| POST | `/auth/login` | `{email, password, device_name?}` | 200 token + user |
| POST | `/auth/refresh` | `{refresh_token}` | 200 token baru |
| POST | `/auth/logout` | Bearer atau `{refresh_token}` | 204 |
| POST | `/auth/logout-all` 🔒 | – | 204 |
| GET | `/me` 🔒 | – | 200 user |
| PATCH | `/me/profile` 🔒 | `{first_name?, last_name?, phone?, gender?}` (hanya yang dikirim berubah) | 200 user |
| GET | `/me/addresses` 🔒 | – | 200 daftar alamat (utama dulu) |
| POST | `/me/addresses` 🔒 | `{label, recipient_name, phone, province, city, district, village, postal_code, street_address, latitude?, longitude?, maps_url?, is_primary?, shipping_area_id?}` | 201 alamat |
| PATCH | `/me/addresses/{id}` 🔒 | field yang ingin diubah | 200 alamat |
| DELETE | `/me/addresses/{id}` 🔒 | – | 204 |
| POST | `/me/addresses/{id}/primary` 🔒 | – | 200 alamat |

Area pengiriman: cari dengan `GET /shipping/areas?q=` (lihat bawah), lalu kirim
`shipping_area_id` di alamat. Hanya id yang ditemukan server sendiri yang diterima (id
karangan atau dari penyedia lain → `422`, `fields.shipping_area_id`). Bila area dipilih,
`province`, `city`, `district`, `postal_code` diambil dari area (boleh tidak dikirim).
`shipping_area_id: ""` menghapus area. Alamat berisi `shipping_area_id` dan
`shipping_area_label` (null bila belum dipilih; checkout lalu tidak bisa menghitung ongkir).

Contoh login:

```http
POST /api/v1/auth/login
Content-Type: application/json

{"email": "siti@example.com", "password": "rahasia123", "device_name": "Pixel 8"}
```

```json
{
  "data": {
    "access_token": "ashat_…",
    "refresh_token": "ashrt_…",
    "token_type": "Bearer",
    "expires_in": 900,
    "refresh_expires_in": 2592000,
    "user": {
      "id": 42, "email": "siti@example.com", "first_name": "Siti", "last_name": "Rahma",
      "phone": "+6281234567890", "gender": "PEREMPUAN", "created_at": "2026-11-03T09:12:00+07:00"
    }
  }
}
```

### Katalog (publik)

| Method | Path | Query | Sukses |
|---|---|---|---|
| GET | `/categories` | – | kategori aktif |
| GET | `/home` | – | `{categories, flash_sale, best_sellers, recommended}` |
| GET | `/products` | `q` (cari), `category` (slug), `min_price`, `max_price`, `flash_sale`, `sort` (`relevance`\|`newest`\|`bestselling`\|`price_asc`\|`price_desc`), `page`, `per_page` | daftar kartu + `meta.total`, `meta.sort` |
| GET | `/products/{slug}` | – | detail produk |
| GET | `/products/{slug}/reviews` | `page` | ulasan publik (nama disamarkan) + `meta.rating` |

Pencarian adalah `GET /products?q=…` (tidak ada endpoint pencarian paralel). Produk
nonaktif, kategori nonaktif, dan produk tanpa varian aktif tidak pernah tampil (404).

Detail produk (ringkas):

```json
{
  "data": {
    "id": 7, "slug": "kaos-ashanum-basic", "name": "Kaos Ashanum Basic", "brand": "Ashanum",
    "description": "…", "category": {"id": 1, "name": "Atasan", "slug": "atasan", "image_url": null},
    "images": [{"url": "https://res.cloudinary.com/…/c_limit,w_900,f_auto,q_auto/…", "thumbnail_url": "…w_200…", "alt": "…"}],
    "rating": {"average": 4.8, "count": 56}, "sold_count": 120, "flash_sale": true,
    "price": {"min": 129000, "max": 129000, "compare_at_price": 159000}, "in_stock": true,
    "attributes": [{"id": 3, "name": "Warna", "values": [{"id": 10, "value": "Hitam"}]}],
    "variants": [{
      "id": 21, "sku": "ASH-BLK-M", "attributes": {"Warna": "Hitam", "Ukuran": "M"}, "value_ids": [10, 12],
      "price": 129000, "compare_at_price": 159000, "discount_percent": 19, "stock": 10, "available": true
    }]
  }
}
```

`available` dihitung server; aplikasi tidak perlu menghitung ketersediaan sendiri.

### Keranjang 🔒

| Method | Path | Body | Sukses |
|---|---|---|---|
| GET | `/cart` | – | 200 keranjang |
| POST | `/cart/items` | `{variant_id, quantity}` (varian sama digabung) | 201 keranjang |
| PATCH | `/cart/items/{id}` | `{quantity}` | 200 keranjang |
| DELETE | `/cart/items/{id}` | – | 200 keranjang |
| DELETE | `/cart` | – | 204 |

Setiap mutasi mengembalikan keranjang lengkap (tidak perlu request kedua). Item berisi
`id, product_id, product_slug, product_name, variant_id, variant_label, sku, image_url,
unit_price, compare_at_price, quantity, stock, max_quantity, available, status, subtotal`;
`summary` menjumlahkan item yang bisa di-checkout. Harga di keranjang hanya tampilan;
checkout menghitung ulang.

### Pengiriman 🔒

| Method | Path | Body / query | Sukses |
|---|---|---|---|
| GET | `/shipping/areas` | `q` (kecamatan/kota/kode pos, 3–60 huruf) | 200 `[{id, provider, label, province, city, district, postal_code, latitude, longitude}]` |
| POST | `/shipping/rates` | `{cart_item_ids}` **atau** `{buy_now}`, `address_id` (wajib), `shipping_quote_id?` | 200 blok `shipping` (lihat preview) |

`/shipping/rates` = bagian ongkir dari preview, tetapi menjawab error bila tidak ada opsi:
`422 SHIPPING_AREA_REQUIRED`, `409 ITEMS_NOT_SHIPPABLE`, `422 NO_COURIER`,
`503 SHIPPING_PROVIDER_UNAVAILABLE`. Preview tetap 200 dengan `shipping.status` yang
menjelaskan (agar keranjang tetap bisa ditampilkan).

### Checkout & pesanan 🔒

| Method | Path | Body / query | Sukses |
|---|---|---|---|
| POST | `/checkout/preview` | `{cart_item_ids}` **atau** `{buy_now: {variant_id, quantity}}`, `address_id?`, `shipping_quote_id?` | 200 preview |
| POST | `/orders` | sama + `address_id`, `shipping_quote_id` (wajib), `idempotency_key` (8–64, wajib), `expected_total?` | 201 pesanan (200 bila key sama) |
| GET | `/orders` | `status` (`unpaid`\|`processing`\|`shipped`\|`completed`\|`cancelled`), `page` | daftar |
| GET | `/orders/{order_number}` | – | detail |
| POST | `/orders/{order_number}/cancel` | – | 200 detail (hanya pesanan belum bayar) |
| POST | `/orders/{order_number}/payment` | – | 200 `{redirect_url, payment, sandbox}` |
| GET | `/orders/{order_number}/payment` | – | 200 status terkini (dicek ke Midtrans) |
| GET | `/orders/{order_number}/tracking` | – | 200 pengiriman + riwayat (dari database) |

- Beli Sekarang (`buy_now`) tidak pernah masuk keranjang.
- Harga, stok, dan total **selalu dari server**. `expected_total` hanya penjaga: jika total
  berubah sejak preview → `409 PRICE_CHANGED` (tampilkan preview baru).
- `address_id` milik pengguna lain → `422` (tidak pernah diganti diam-diam).
- Ongkos kirim dihitung server dari berat katalog dan area alamat (penyedia: Biteship,
  mode testing). Preview memuat `shipping_cost`, `total` (sudah termasuk ongkir) dan:

  ```json
  "shipping": {
    "status": "READY", "message": null, "cost": 9000, "weight_grams": 200, "changed": false,
    "selected": {"quote_id": "8f0c…", "provider": "biteship", "courier_code": "jne", "courier_name": "JNE",
                 "service_code": "reg", "service_name": "Reguler", "description": "", "price": 9000,
                 "estimated_days": "2-3", "estimate_label": "2–3 hari", "expires_at": "2026-11-05T10:45:00+07:00"},
    "options": [ … urut termurah … ]
  }
  ```

  `status`: `READY`, `ADDRESS_REQUIRED`, `AREA_REQUIRED` (alamat tanpa area),
  `ITEMS_BLOCKED` (ada produk bermasalah/tanpa berat), `NO_COURIER`,
  `PROVIDER_UNAVAILABLE`, `NOT_CONFIGURED`. Selain READY, `ready=false`, `cost=0`
  (**bukan gratis ongkir**, pesanan tidak bisa dibuat) dan `warnings` memuat
  `SHIPPING_<status>`.
- Pilihan kurir = `shipping_quote_id`. Kirim id yang sama ke preview berikutnya untuk tetap
  memilihnya (harga & kedaluwarsa diperbarui); id yang tidak berlaku → opsi termurah dipilih,
  `changed: true`, warning `SHIPPING_CHANGED`. Ganti alamat/jumlah → preview ulang.
- `POST /orders` menerima **hanya** `shipping_quote_id`; tidak ada `shipping_cost` dari klien
  (`400 UNKNOWN_FIELD`). Quote kedaluwarsa (default 30 menit), sudah dipakai, milik orang
  lain, atau alamat/paket berubah → `409 SHIPPING_CHANGED`. Total = subtotal + ongkir quote.
- Detail pesanan memuat `items[].id` (untuk ulasan/retur), `returnable_quantity`,
  `review_id`, `shipping_cost`, `shipping` (snapshot pilihan checkout: `cost, provider,
  courier_code, courier_name, service_code, service_name, estimated_days, estimate_label,
  weight_grams, area_label`; semua null kecuali `cost: 0` untuk pesanan lama), `shipment`
  (`method` PROVIDER/MANUAL, `status`, `status_label`, `courier`, `service`,
  `tracking_number`, `tracking_url`, `shipped_at`, `delivered_at`, `live_tracking`), `latest_payment`,
  `actions {can_pay, can_cancel, can_review, can_return}`, `return_deadline`, `returns`.
- `GET /orders/{n}/tracking` → `{order_number, order_status, order_status_label, shipping,
  shipment, events: [{status, status_label, description, location, time}]}` (terbaru dulu).
  Status pengiriman: `CREATED` (menunggu penjemputan), `PICKED_UP`, `IN_TRANSIT`,
  `OUT_FOR_DELIVERY`, `DELIVERED`, `EXCEPTION`, `RETURNED`, `CANCELLED`. `DELIVERED`
  tidak membuat pesanan COMPLETED (penjual menyelesaikannya; jendela retur mulai saat itu).
  Data diperbarui oleh webhook penyedia / penjual; endpoint ini tidak memanggil penyedia,
  cukup dibaca ulang (tidak ada push).

Pembayaran: Midtrans **Sandbox saja**, sebesar `total` pesanan (barang + ongkir). `redirect_url` dibuka di browser/in-app browser.
Midtrans mengembalikan pembeli ke `API_PAYMENT_FINISH_URL` (app link yang diset server;
klien tidak bisa memilih URL redirect). Kembali ke aplikasi **tidak** mengubah status:
status lunas hanya dari notifikasi Midtrans yang diverifikasi atau pengecekan status ke
Midtrans (`GET …/payment`).

### Ulasan & retur 🔒

| Method | Path | Body / query | Sukses |
|---|---|---|---|
| GET | `/me/reviews` | `tab` (`pending` = belum diulas), `page` | daftar + `meta.pending_count` |
| POST | `/reviews` | `{order_item_id, rating (1–5), comment?}` | 201 ulasan |
| PATCH | `/reviews/{id}` | `{rating?, comment?}` | 200 ulasan |
| GET | `/me/returns` | `page` | daftar |
| GET | `/me/returns/{return_number}` | – | detail + timeline |
| POST | `/returns` | `{order_number, items: [{order_item_id, quantity}], type (REFUND\|EXCHANGE), reason, description?, evidence? (URL https, maks. 5), idempotency_key?}` | 201 retur |
| POST | `/me/returns/{return_number}/cancel` | – | 200 retur (hanya selama REQUESTED) |

Ulasan dan retur hanya untuk pesanan COMPLETED milik sendiri; retur juga harus sudah dibayar
dan masih dalam batas waktu (`return_deadline`). Nilai refund dihitung server dari harga
yang dibayar.

## Kebijakan lain

- Tidak ada GraphQL, WebSocket, atau push; status dibaca ulang dengan GET.
- Sesi API kedaluwarsa dibersihkan saat login (lazy) dan lewat
  `ecommerce cleanup-api-sessions` (cron/systemd timer).
- Versi: semua di `/api/v1`. Perubahan yang tidak kompatibel akan masuk `/api/v2`.
