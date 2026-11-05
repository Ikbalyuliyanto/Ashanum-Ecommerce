# Alur aplikasi mobile (rencana integrasi)

Dokumen ini menjelaskan bagaimana aplikasi mobile kelak memakai [API v1](api-v1.md).
Belum ada aplikasi Flutter/Android/iOS; yang tersedia adalah backend yang siap dipakai.

```text
Register / Login
      ↓
Home ─────────── GET /home, GET /categories
      ↓
Produk ───────── GET /products?q=&category=&sort= , GET /products/{slug}
      ↓
Keranjang ────── POST /cart/items, GET /cart  (atau Beli Sekarang tanpa keranjang)
      ↓
Checkout ─────── POST /checkout/preview  →  POST /orders (idempotency_key)
      ↓
Pesanan ──────── GET /orders/{order_number}
      ↓
Pembayaran ───── POST /orders/{order_number}/payment  →  buka redirect_url
      ↓
Kembali ke app ─ app link API_PAYMENT_FINISH_URL (deep link)
      ↓
Refresh status ─ GET /orders/{order_number}/payment  (server cek ke Midtrans)
```

## 1. Register / Login

1. `POST /auth/register` atau `POST /auth/login` dengan `device_name` (mis. model HP).
2. Simpan `access_token` di memori dan `refresh_token` di penyimpanan aman OS
   (Android Keystore / iOS Keychain). Jangan simpan password.
3. Setiap request: `Authorization: Bearer <access_token>`.
4. Saat menerima `401 TOKEN_EXPIRED`: jalankan **satu** `POST /auth/refresh`, ganti kedua
   token dengan yang baru, lalu ulangi request yang tertunda. Request lain menunggu refresh
   selesai (jangan dua refresh paralel — refresh token lama yang dipakai lagi dianggap
   pencurian dan mencabut sesi perangkat: `401 REFRESH_TOKEN_REUSED`).
5. `401 INVALID_TOKEN` atau `REFRESH_TOKEN_REUSED` → hapus token, tampilkan login.
6. Logout: `POST /auth/logout` (Bearer, atau body `refresh_token`). "Keluar dari semua
   perangkat": `POST /auth/logout-all`. Sesi web browser tidak terpengaruh.

## 2. Home & produk

- `GET /home` memberi kategori + flash sale + terlaris + rekomendasi dalam satu request.
- Daftar/cari: `GET /products` (paginasi `page`, `per_page`, `meta.has_more`).
- Detail: `GET /products/{slug}`. Pemilih varian memakai `attributes` dan `variants`
  (`attributes` per varian berupa map nama → nilai, `available` sudah dihitung server).
- Gambar: pakai `image_url` / `thumbnail_url` apa adanya (sudah di-resize Cloudinary).
- Harga integer rupiah: format "Rp129.000" di aplikasi.

## 3. Keranjang & checkout

- Tambah: `POST /cart/items {variant_id, quantity}` → keranjang lengkap.
- Checkout dari keranjang: `POST /checkout/preview {cart_item_ids, address_id?}`.
  Beli Sekarang: `POST /checkout/preview {buy_now: {variant_id, quantity}}`.
- Tampilkan `items[].problem` dan `warnings`; tombol "Buat Pesanan" hanya jika `ready`.
- Ongkos kirim: tampilkan `shipping.message` (belum dihitung) — jangan menulis "Gratis".
- Buat pesanan: `POST /orders` dengan `idempotency_key` acak per percobaan checkout
  (UUID tanpa tanda kurung kurawal, 8–64 karakter) dan `expected_total` dari preview.
  Jika koneksi putus, kirim ulang dengan key yang sama: server mengembalikan pesanan yang
  sama (`200`), bukan pesanan ganda.
- `409 PRICE_CHANGED` / `STOCK_CHANGED` → ambil preview baru dan minta konfirmasi lagi.

## 4. Pembayaran (Midtrans Sandbox)

1. `POST /orders/{order_number}/payment` → `redirect_url`.
2. Buka di Custom Tab (Android) / `ASWebAuthenticationSession` atau SFSafariViewController
   (iOS). Jangan menyisipkan JavaScript Snap di aplikasi.
3. Midtrans mengarahkan kembali ke `API_PAYMENT_FINISH_URL` yang diset server, misalnya:

   ```text
   https://ashanum.example/app/payment-finish?order={order_number}   (App Link / Universal Link)
   ashanum://payment/finish?order={order_number}                     (custom scheme)
   ```

   `{order_number}` diganti server. Klien **tidak** bisa mengirim URL redirect sendiri.
   Placeholder di atas belum terdaftar di toko aplikasi; untuk App Link/Universal Link
   nanti perlu `/.well-known/assetlinks.json` dan `apple-app-site-association` di domain.
4. Saat aplikasi terbuka dari deep link (atau pengguna menutup browser), panggil
   `GET /orders/{order_number}/payment`. Server menanyakan status ke Midtrans; parameter di
   URL kembali **tidak pernah** dipercaya. Tampilkan `paid`/`order_status`.
5. Status final datang dari notifikasi Midtrans (webhook) atau pengecekan status; aplikasi
   cukup membaca ulang.

## 5. Setelah pesanan

- Riwayat: `GET /orders?status=…`. Detail: `GET /orders/{order_number}` (`actions`,
  `shipment`, `returns`).
- Pesanan selesai (`actions.can_review`): `POST /reviews {order_item_id, rating, comment}`.
- Pengembalian (`actions.can_return`, sebelum `return_deadline`):
  `POST /returns {order_number, items, type, reason, …}`; pantau `GET /me/returns/{n}`.

## Belum termasuk (backlog)

- Aplikasi Flutter/Android/iOS, push notification, App Link/Universal Link terdaftar.
- Integrasi ongkir/kurir (biaya kirim masih 0, pelacakan manual).
- Midtrans produksi dan refund otomatis.
- Layar "perangkat aktif" (data `device_name`/`last_used_at` sudah disimpan).
- Pengaturan toko/admin dan modul pelanggan (CRM) — ditunda sejak Fase 8.
