# Pengiriman (Fase 10)

Ongkos kirim dihitung dari penyedia pengiriman (Biteship), dipilih pembeli saat checkout,
masuk ke total pesanan, lalu dibooking oleh toko setelah pesanan dibayar dan diproses.
Status paket diperbarui dari webhook penyedia, tombol "Perbarui Status" di admin, atau
perintah `reconcile-shipments`. Pengiriman manual (Fase 8) tetap ada sebagai cadangan.

Status saat ini: **mode testing Biteship saja**. Integrasi belum pernah diuji dengan API
Biteship asli (belum ada key); semua tes memakai tiruan API Biteship yang mengikuti field
dokumentasi resmi. `BITESHIP_ENV=live` ditolak kecuali `SHIPPING_LIVE_ENABLED=true`.

## 1. Arsitektur

```text
checkout / order / admin / API v1
              │
      shipping.Service      (domain: area, quote, booking, tracking, webhook, reconcile)
              │
      shipping.Provider     (interface)
              │
      biteship.Client       (adapter net/http; nanti: RajaOngkir/Komerce sebagai adapter lain)
```

- `internal/shipping` — tipe netral (Area, Rate, Quote, Package, Shipment, Status, Event),
  `Service`, aturan status, handler webhook. Tidak menyimpan JSON mentah penyedia.
- `internal/shipping/biteship` — satu-satunya kode yang tahu format Biteship.
- `internal/shipping/biteship/biteshiptest` — tiruan API Biteship (httptest) untuk tes.
- `internal/shipping/shippingtest` — perlengkapan tes (service + fake + konfigurasi).

Interface `Provider`:

| Method | Biteship |
|---|---|
| `SearchAreas(query)` | `GET /v1/maps/areas?countries=ID&input=…&type=single` |
| `Rates(req)` | `POST /v1/rates/couriers` |
| `CreateShipment(booking)` | `POST /v1/orders` (+ `GET /v1/orders/:id` bila referensi sudah dipakai) |
| `Tracking(id, trackingID)` | `GET /v1/trackings/:tracking_id` |
| `CancelShipment(id, reason)` | `POST /v1/orders/:id/cancel` |
| `ParseWebhook(header, body)` | verifikasi header rahasia + normalisasi event |

Menambah penyedia lain = satu paket adapter baru yang memenuhi interface ini; checkout,
pesanan, dan admin tidak berubah. Area, quote, dan shipment menyimpan nama penyedianya.

## 2. Konfigurasi Biteship

| Variabel | Default | Keterangan |
|---|---|---|
| `SHIPPING_PROVIDER` | `biteship` | Satu-satunya nilai yang diterima saat ini |
| `BITESHIP_API_KEY` | kosong | Backend saja. Kosong = pengiriman mati: checkout tidak bisa membuat pesanan |
| `BITESHIP_ENV` | `test` | `test` butuh key `biteship_test.*`; `live` butuh `biteship_live.*` |
| `SHIPPING_LIVE_ENABLED` | `false` | Wajib `true` untuk `live`. Biarkan `false` sampai alur diuji dengan key testing |
| `BITESHIP_WEBHOOK_HEADER` | `X-Ashanum-Webhook-Secret` | Nama header ("Headers Signature Key" di dashboard) |
| `BITESHIP_WEBHOOK_SECRET` | kosong | Nilai header (min. 24 karakter). Kosong = semua webhook ditolak |
| `BITESHIP_BASE_URL` | kosong | Hanya development (API tiruan lokal). Ditolak di production |
| `SHIP_FROM_NAME`, `SHIP_FROM_PHONE`, `SHIP_FROM_ADDRESS`, `SHIP_FROM_POSTAL_CODE` | kosong | Alamat toko (wajib bila key diisi) |
| `SHIP_FROM_AREA_ID`, `SHIP_FROM_LATITUDE`, `SHIP_FROM_LONGITUDE` | kosong | Opsional; area id dari pencarian area |
| `SHIPPING_COURIERS` | `jne,sicepat,jnt,anteraja,pos` | Kode kurir yang ditawarkan |
| `SHIPPING_RATE_CACHE_TTL` | `10m` | Cache tarif di memori (5–15 menit) |
| `SHIPPING_QUOTE_TTL` | `30m` | Umur quote untuk membuat pesanan (5m–2h) |

Key hanya dikirim di header `Authorization` ke Biteship; tidak pernah dirender ke HTML,
dikirim ke aplikasi mobile, disimpan di database, atau ditulis ke log (pesan error
konfigurasi juga tidak memuat key).

## 3. Area alamat

Alamat menyimpan area pengiriman netral: `shipping_area_provider`, `shipping_area_id`,
`shipping_area_label` (FK ke `shipping_areas`). Tidak ada kolom `biteship_area_id`.

1. Pembeli mengetik kecamatan / kota / kode pos (min. 3 huruf) di form alamat. Web: HTMX
   `GET /account/addresses/areas?area_q=` (tanpa JavaScript: tombol "Cari" mengirim form
   dengan `intent=search_area`, tidak menyimpan). API: `GET /api/v1/shipping/areas?q=`.
2. Hasil disimpan ke cache `shipping_areas` (provider, id, label, provinsi, kota,
   kecamatan, kode pos, koordinat opsional). Pencarian sama diulang dari memori 1 jam
   (Maps API Biteship berkuota bulanan); endpoint pencarian dibatasi per IP.
3. Saat menyimpan, `shipping_area_id` harus ada di cache untuk penyedia aktif: id karangan
   atau id penyedia lain ditolak (`fields.shipping_area_id`). Provinsi, kota, kecamatan,
   dan kode pos alamat diambil dari area itu sehingga selalu cocok; kelurahan, jalan, dan
   koordinat tetap dari pembeli.
4. Alamat lama (sebelum Fase 10) tidak punya area: checkout menampilkan "Lengkapi Lokasi
   Pengiriman" dan tidak menghitung tarif. Checkout tidak pernah memanggil pencarian area.

## 4. Berat produk

- `products.weight_grams` (wajib > 0 untuk produk aktif), dimensi opsional `length_cm`,
  `width_cm`, `height_cm`; `product_variants.weight_grams` opsional menimpa berat produk.
- Berat diisi admin di form produk/varian (field eksplisit; tidak ada mass assignment).
  Produk aktif tanpa berat ditolak saat disimpan. Produk lama berberat 0 diberi label
  "berat belum diatur" di daftar admin dan peringatan di halaman ubah; varian tanpa berat
  tampil "belum bisa dikirim" di checkout dan tidak bisa dipesan
  (`ITEMS_NOT_SHIPPABLE`). Tidak ada asumsi 1 kg.
- Berat paket = Σ berat per unit × jumlah. Berat per unit disalin ke `order_items.weight_grams`
  saat checkout, sehingga booking mengirim berat yang sama dengan yang di-quote.
- Klien tidak pernah mengirim berat.

## 5. Alur tarif dan quote

```text
alamat (area) + item (berat dari katalog)
      │
      ▼
Service.Quotes ── cache tarif (memori, kunci: penyedia + asal + tujuan + berat/dimensi + kurir, TTL 10 menit)
      │               └─ miss → Provider.Rates (origin area/kode pos, destination area, items, couriers)
      ▼
UPSERT shipping_quotes (satu statement) → [quote_id, kurir, layanan, harga, estimasi, expires_at]
```

- Satu quote = satu penawaran (kurir + layanan) untuk satu pembeli, rute, dan paket.
  `quote_key` = hash(penyedia, asal, tujuan, paket, kurir, layanan): memuat ulang tarif
  memperbarui quote yang sama (id tetap, harga & kedaluwarsa mengikuti penyedia) selama
  belum dipakai. Quote terpakai disimpan sebagai catatan biaya yang ditagih.
- Domain tarif ternormalisasi: `provider, courier_code, courier_name, service_code,
  service_name, description, price, estimated_days` ("2-3", "1", "0" = hari ini).
- Penawaran tanpa harga atau tanpa kode dibuang; tidak ada kurir → `NO_COURIER`.
- Gagal hubungi penyedia (jaringan, timeout 15 dtk, 5xx, JSON rusak, ditolak) →
  "Pengiriman sementara tidak dapat dihitung. Silakan coba lagi." / API
  `503 SHIPPING_PROVIDER_UNAVAILABLE`. **Tidak pernah ongkir 0 sebagai cadangan.**

### Kedaluwarsa & validasi saat membuat pesanan

Di transaksi pembuatan pesanan, quote dikunci (`FOR UPDATE`) dan harus:

- milik pembeli, belum dipakai, belum kedaluwarsa,
- penyedia, asal (alamat toko), tujuan (area + kode pos + koordinat alamat) sama,
- paket sama (varian, jumlah, berat, dimensi).

Selain itu → `409 SHIPPING_CHANGED` (web: halaman checkout dimuat ulang dengan tarif
baru). Total = subtotal + harga quote; `expected_total` yang berbeda → `409 PRICE_CHANGED`.
Statement pembuatan pesanan menandai quote terpakai; bila sudah terpakai oleh pesanan lain
seluruh transaksi dibatalkan. Quote tak terpakai yang kedaluwarsa > 1 hari dihapus oleh
`reconcile-shipments`.

## 6. Checkout

Web (`/checkout`): kartu Pengiriman menampilkan pilihan terpilih
("AnterAja Regular · Estimasi tiba 3–4 hari · Rp7.500 · Ubah"). "Ubah" membuka sheet
(bottom sheet di mobile, dialog di desktop, `<details>` tanpa JS) berisi semua opsi;
memilih opsi memuat ulang checkout dari server via HTMX (`?ship=<quote_id>`, tanpa JS:
tombol noscript). Default = termurah. Form pesanan hanya membawa `shipping_quote_id` dan
`expected_total`. Ganti alamat atau jumlah → tarif dihitung ulang di server; harga dari
browser tidak pernah dipakai.

API: `POST /checkout/preview` (atau `POST /shipping/rates`) → `shipping.options[].quote_id`
→ `POST /orders {…, shipping_quote_id}`. Tidak ada field `shipping_cost` di request
(field tak dikenal → `400 UNKNOWN_FIELD`). Web dan API memakai `checkout.Service` yang sama.

## 7. Snapshot pesanan

Disalin dari quote ke `orders` saat pesanan dibuat dan tidak pernah berubah (tidak ada
alur admin untuk mengubahnya): `shipping_provider`, `shipping_courier_code/name`,
`shipping_service_code/name`, `shipping_estimated_days`, `shipping_weight_grams`,
`shipping_area_id/label`, `shipping_origin` (alamat toko, JSON), `shipping_cost`.
Tujuan = snapshot alamat yang sudah ada. Tetap terbaca saat penyedia mati.

- `orders.total = subtotal + shipping_cost - discount` (CHECK database).
- Midtrans hanya menagih `orders.total`; `item_details` memuat baris "Ongkos Kirim" dan
  harus berjumlah sama dengan total. Layanan pembayaran tidak menghitung ongkir.
- Pesanan lama (sebelum Fase 10): `shipping_provider` NULL, ongkir 0, tidak diubah.
- Refund retur tetap hanya nilai barang (ongkir tidak ikut). Pembatalan pesanan berbayar
  sebelum dikirim mencatat refund sebesar yang dibayar (termasuk ongkir).

## 8. Booking pengiriman

Hanya untuk pesanan **dibayar dan PROCESSING** (tidak pernah saat checkout atau
WAITING_PAYMENT). Admin → pesanan → "Buat Pengiriman":

1. Transaksi: kunci pesanan, cek status & snapshot, cek tidak ada pengiriman aktif,
   buat baris `shipments` `PENDING` dengan `booking_key` unik (`<nomor>-S<n>`), catat audit
   `shipment.book`.
2. Panggil `POST /v1/orders` dengan `reference_id = booking_key`.
3. Simpan id pengiriman, tracking id, resi (waybill), tautan lacak → status `CREATED`,
   event pertama.

Idempotensi:

- Index unik "satu pengiriman aktif per pesanan" + baris PENDING: klik ganda / request
  paralel tidak pernah membuat dua pengiriman (request kedua: `ErrBookingPending` atau
  pengiriman yang sudah ada).
- Penyedia tidak terjangkau (hasil tidak diketahui): baris tetap PENDING, pesanan tidak
  berubah. "Coba Lagi" setelah 30 detik mengirim **referensi yang sama**; Biteship menolak
  referensi ganda (kode 40002060) dan menyebut pesanan yang sudah ada, yang lalu dipakai.
- Penyedia menolak (4xx): baris `FAILED` dengan alasan; pesanan tetap PROCESSING; booking
  berikutnya memakai referensi baru (`-S2`).
- `reconcile-shipments` melaporkan booking PENDING > 10 menit.

Batal sebelum dijemput: "Batalkan pengiriman" (penyedia dulu, lalu `CANCELLED`, audit
`shipment.cancel`); booking PENDING yang dibatalkan ditandai FAILED. Pesanan tetap
PROCESSING. Pesanan dengan booking aktif tidak bisa dibatalkan admin sebelum booking-nya
dibatalkan. Setelah dijemput penyedia menolak pembatalan.

## 9. Fallback manual

Form "Kirim manual" (Fase 8) tetap ada: dipakai bila pesanan lama tanpa snapshot, penyedia
gangguan, atau toko memakai kurir lain. Membuat pengiriman `MANUAL` (`IN_TRANSIT`) dan
pesanan langsung SHIPPED. Ditolak bila sudah ada pengiriman aktif dari penyedia. Pengiriman
manual tidak punya pelacakan otomatis.

## 10. Status & tracking

Status internal (`shipments.status`), dipetakan dari status Biteship (snake_case maupun
camelCase) — status penyedia tidak pernah menjadi status bisnis:

| Internal | Biteship |
|---|---|
| `CREATED` | confirmed, scheduled, allocated, picking_up |
| `PICKED_UP` | picked |
| `IN_TRANSIT` | in_transit |
| `OUT_FOR_DELIVERY` | dropping_off |
| `DELIVERED` | delivered |
| `EXCEPTION` | on_hold, rejected, return_in_transit, disposed |
| `RETURNED` | returned |
| `CANCELLED` | cancelled, courier_not_found |

Ditambah `PENDING` (booking belum dijawab) dan `FAILED` (ditolak). Status turunan dari
semua event: kemajuan tidak pernah mundur (event terlambat/ganda/acak urutan aman),
RETURNED/CANCELLED final, EXCEPTION berlaku hanya bila event terbaru sebelum DELIVERED.

Status pesanan dan pengiriman terpisah:

- PICKED_UP / IN_TRANSIT / OUT_FOR_DELIVERY / DELIVERED → pesanan PROCESSING menjadi SHIPPED.
- **DELIVERED tidak menyelesaikan pesanan.** Pesanan tetap SHIPPED sampai toko menekan
  "Selesaikan Pesanan" (atau kebijakan otomatis kelak), karena saat COMPLETED jendela
  retur dimulai; admin melihat "Kurir melaporkan paket terkirim".
- EXCEPTION / RETURNED / CANCELLED tidak mengubah status pesanan (ditangani admin).

`shipment_events`: satu baris per (pengiriman, `event_key`), dengan `event_key` =
`status:<status penyedia>`; event yang sama datang berkali-kali disimpan sekali dan
waktunya diambil yang paling awal (webhook = waktu terima, tracking = waktu penyedia).

Tampilan: `/account/orders/{nomor}` (kurir, layanan, estimasi, resi, status, tautan lacak,
timeline) dan `GET /api/v1/orders/{nomor}/tracking`. Keduanya hanya membaca database;
membuka halaman **tidak pernah** memanggil penyedia.

## 11. Webhook

`POST /webhooks/shipping/biteship` — publik, tanpa sesi/CSRF browser (request
server-ke-server tanpa header Origin), body maks. 64 KiB, rate limit 300/menit per IP.

- Keaslian: header `BITESHIP_WEBHOOK_HEADER` harus sama dengan `BITESHIP_WEBHOOK_SECRET`
  (perbandingan constant-time) — mekanisme "Headers Signature Key/Secret" Biteship. Tanpa
  secret terkonfigurasi semua webhook ditolak.
- Body kosong/`{}` (tes instalasi Biteship) → 200, tanpa efek.
- `order.status` → event status; `order.waybill_id` → resi; `order.price` → diabaikan
  (ongkir pesanan tidak berubah).
- Jawaban: 200 `applied`/`ignored` (pengiriman tidak dikenal dicatat di log dan dijawab
  200 agar tidak diulang terus), 401 rahasia salah, 400 rusak, 413 terlalu besar, 404 bila
  pengiriman tidak dikonfigurasi, 500 error server (penyedia akan mengulang).

Dashboard Biteship: URL `https://<APP_BASE_URL>/webhooks/shipping/biteship`, event
`order.status` dan `order.waybill_id`, header signature key/secret sesuai konfigurasi.

## 12. Rekonsiliasi

```sh
ecommerce reconcile-shipments [-limit 100]
```

Tanpa worker permanen (cron / systemd timer, mis. tiap 30 menit):

- memperbarui pengiriman penyedia yang masih berjalan dan belum disinkron 30 menit
  (terlama dulu) lewat `Tracking`,
- melaporkan booking PENDING > 10 menit (`REVIEW … retry "Buat Pengiriman"`),
- menghapus quote tak terpakai yang kedaluwarsa > 1 hari.

Keluar dengan error bila ada kegagalan (lihat log).

## 13. Belum termasuk

- Uji dengan API Biteship asli (key testing) — tes `TestRealBiteshipTesting` siap, hanya
  pencarian area dan tarif, SKIP tanpa `BITESHIP_TEST_API_KEY`.
- Pengiriman live / pembayaran ke Biteship, COD, asuransi, pickup terjadwal, instan.
- Booking pengiriman retur: barang retur tetap dikirim pembeli sendiri seperti Fase 7–8;
  skema retur belum menyimpan kurir/resi retur (bisa ditambah tanpa mengubah pengiriman).
- Penyedia kedua (RajaOngkir/Komerce): cukup adapter baru.
