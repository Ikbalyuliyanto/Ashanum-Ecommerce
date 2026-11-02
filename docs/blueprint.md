# Blueprint E-Commerce Go — Lightweight HG680P

## 1. Tujuan

Membangun ulang aplikasi e-commerce menjadi aplikasi yang:

- Sangat ringan untuk HG680P RAM 2 GB.
- Backend dan frontend berada dalam satu aplikasi Go.
- Database PostgreSQL berada di server terpisah.
- Tidak menggunakan React, Next.js, Node.js, Prisma, atau frontend SPA berat.
- Server-side rendering menggunakan Go Template.
- Interaksi dinamis menggunakan HTMX.
- JavaScript hanya digunakan apabila benar-benar diperlukan.
- Deployment production cukup menggunakan satu binary Go.
- Nginx digunakan sebagai reverse proxy.
- Gambar produk disimpan di Cloudinary/object storage, bukan membebani storage HG680P.

---

# 2. Technology Stack

## Backend

```text
Go
Chi Router
pgx
sqlc
PostgreSQL
```

Chi dapat diganti dengan `net/http` apabila ingin dependency lebih sedikit.

## Frontend

```text
Go html/template
HTMX
CSS
Sedikit Vanilla JavaScript
```

Tidak menggunakan:

```text
React
Next.js
Vue
Angular
Node.js frontend runtime
```

## Infrastruktur

```text
Cloudflare
    ↓
Nginx
    ↓
Go Application
    ↓
PostgreSQL Remote
```

External services:

```text
Midtrans
Google OAuth
Cloudinary
SMTP / Email Provider
```

---

# 3. Arsitektur

Gunakan:

```text
Modular Monolith
```

Bukan microservices.

Semua domain tetap berada di satu project dan menghasilkan satu binary:

```text
ecommerce
```

Arsitektur:

```text
HTTP Request
      ↓
Handler
      ↓
Service
      ↓
Repository
      ↓
PostgreSQL
```

Aturan:

```text
Handler
tidak boleh langsung query database.

Service
menangani business logic.

Repository
menangani query/database.

Model/Domain
menyimpan struktur domain.

Template
menangani tampilan HTML.
```

---

# 4. Struktur Folder

```text
ecommerce-go/
│
├── cmd/
│   └── web/
│       └── main.go
│
├── internal/
│
│   ├── config/
│   │   └── config.go
│
│   ├── database/
│   │   ├── postgres.go
│   │   ├── migrations/
│   │   └── queries/
│
│   ├── middleware/
│   │   ├── auth.go
│   │   ├── admin.go
│   │   ├── logging.go
│   │   ├── recovery.go
│   │   ├── csrf.go
│   │   └── rate_limit.go
│
│   ├── auth/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── user/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── category/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── product/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── cart/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── checkout/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── order/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── payment/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── midtrans.go
│   │   ├── webhook.go
│   │   └── routes.go
│
│   ├── shipping/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── review/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   └── routes.go
│
│   ├── return/
│   │   ├── handler.go
│   │   ├── service.go
│   │   ├── repository.go
│   │   ├── model.go
│   │   └── routes.go
│
│   ├── upload/
│   │   ├── service.go
│   │   └── cloudinary.go
│
│   └── admin/
│       ├── dashboard/
│       ├── product/
│       ├── category/
│       ├── order/
│       ├── payment/
│       ├── user/
│       ├── return/
│       └── report/
│
├── web/
│
│   ├── templates/
│   │
│   │   ├── layouts/
│   │   │   ├── base.html
│   │   │   ├── user.html
│   │   │   └── admin.html
│   │   │
│   │   ├── partials/
│   │   │   ├── header.html
│   │   │   ├── navbar.html
│   │   │   ├── footer.html
│   │   │   ├── product-card.html
│   │   │   ├── pagination.html
│   │   │   └── notification.html
│   │   │
│   │   ├── home/
│   │   ├── auth/
│   │   ├── product/
│   │   ├── cart/
│   │   ├── checkout/
│   │   ├── order/
│   │   ├── user/
│   │   └── admin/
│
│   └── static/
│       ├── css/
│       ├── js/
│       ├── images/
│       └── icons/
│
├── migrations/
│
├── sql/
│   ├── queries/
│   └── schema/
│
├── scripts/
│
├── .env.example
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

> Catatan implementasi Fase 1: lihat README bagian "Perbedaan dari struktur di blueprint".

---

# 5. Modul Sistem

## Modul 1 — Authentication

Fitur:

```text
Register
Login
Logout
Google Login
Forgot Password
Reset Password
Session
Role ADMIN / USER
```

Gunakan session berbasis secure cookie.

Hindari JWT apabila aplikasi hanya menggunakan server-rendered web.

---

# 6. User

Fitur:

```text
Profile
Alamat
Daftar alamat
Alamat utama
Riwayat pesanan
Pengembalian
Review
```

---

# 7. Product

Struktur:

```text
Product
├── Category
├── Images
├── Attributes
├── Attribute Values
└── Variants
```

Product mempunyai:

```text
Name
Slug
Description
Brand
Price
OriginalPrice
Discount
Stock
MainImage
Active
FlashSale
BestSeller
Featured
Rating
SoldCount
```

---

# 8. Category

Fitur:

```text
Create Category
Update Category
Delete Category
Category listing
Category product listing
```

---

# 9. Product Variant

Contoh:

```text
Kaos

Warna
├── Hitam
├── Putih

Ukuran
├── S
├── M
├── L
```

Variant:

```text
Hitam + M

SKU
Stock
Price
```

---

# 10. Cart

Fitur:

```text
Add to cart
Update quantity
Remove item
Select variant
Calculate subtotal
```

HTMX dapat digunakan sehingga update cart tidak reload seluruh halaman.

---

# 11. Checkout

Flow:

```text
Cart
 ↓
Address
 ↓
Shipping
 ↓
Order Summary
 ↓
Payment Method
 ↓
Create Order
 ↓
Payment
```

Checkout harus menggunakan database transaction.

---

# 12. Order

Status:

```text
WAITING_PAYMENT
WAITING_CONFIRMATION
PROCESSING
SHIPPED
COMPLETED
CANCELLED
```

Order menyimpan snapshot:

```text
Product name
Product price
Variant
Address
Shipping fee
Discount
Total
```

Jangan hanya bergantung kepada data product aktif karena product dapat berubah setelah transaksi.

---

# 13. Payment

Integrasi:

```text
Midtrans
```

Payment flow:

```text
Create Order
 ↓
Create Midtrans transaction
 ↓
Receive Snap Token
 ↓
Customer Payment
 ↓
Midtrans Webhook
 ↓
Verify signature
 ↓
Update Payment
 ↓
Update Order
```

Webhook harus idempotent.

---

# 14. Shipping

Master:

```text
Courier
Shipping Service
Shipping Cost
Estimated Delivery
```

Tahap awal dapat menggunakan master internal.

Nanti dapat dikembangkan ke API kurir eksternal.

---

# 15. Review

Syarat:

```text
User harus pernah membeli produk.

Order harus selesai.
```

Review:

```text
Rating 1–5
Comment
Photo optional
```

---

# 16. Return / Refund

Flow:

```text
User
 ↓
Submit Return
 ↓
Admin Review
 ↓
Approved / Rejected
 ↓
Return Shipment
 ↓
Admin Verification
 ↓
Refund / Exchange
 ↓
Completed
```

Jenis:

```text
REFUND
EXCHANGE
```

---

# 17. Admin

Menu:

```text
Dashboard

Master
├── Product
├── Category
├── Attribute
├── Variant
├── Courier
└── Shipping Service

Transaction
├── Orders
├── Payments
├── Returns
└── Reviews

User
└── Customers

Reports
├── Sales
├── Products
├── Orders
└── Payments

Settings
└── Store
```

---

# 18. Database

Gunakan PostgreSQL remote.

Minimum tables:

```text
users
user_profiles
user_addresses

categories

products
product_images
product_attributes
product_attribute_values
product_variants
product_variant_values

carts
cart_items

orders
order_items

payments

couriers
shipping_services
shipments

reviews

returns

store_settings
```

---

# 19. Database Access

Gunakan:

```text
pgx
+
sqlc
```

Contoh alur:

```text
SQL query
 ↓
sqlc generate
 ↓
Go typed query
 ↓
Service
 ↓
Handler
```

Hindari ORM besar.

Jangan menggunakan:

```text
GORM
Prisma
```

kecuali ada alasan kuat.

---

# 20. Frontend

Gunakan:

```text
HTML
CSS
HTMX
```

Contoh cart:

```html
<button
    hx-post="/cart/add"
    hx-target="#cart-count">
    Add to Cart
</button>
```

Server mengembalikan HTML partial.

Tidak perlu JSON API untuk seluruh internal frontend.

---

# 21. API

Pisahkan dua jenis endpoint.

Browser:

```text
/products
/cart
/checkout
/orders
/profile
```

External/API:

```text
/api/v1/...
```

Gunakan API hanya untuk:

```text
Payment webhook
future mobile app
external integration
```

Jangan membuat API JSON untuk semua halaman jika tidak diperlukan.

---

# 22. Upload Image

Jangan simpan gambar produk besar di HG680P.

Gunakan:

```text
Browser
 ↓
Go
 ↓
Cloudinary
```

Database hanya menyimpan URL.

Hindari image processing berat menggunakan HG680P.

Jika resize diperlukan, gunakan Cloudinary transformation.

---

# 23. Cache

Tahap awal:

```text
tidak perlu Redis.
```

Gunakan cache memory sederhana untuk:

```text
Store setting
Category
Static configuration
```

Jika trafik meningkat, Redis dapat ditempatkan di server terpisah.

---

# 24. Security

Wajib:

```text
HTTPS
Secure Cookie
HttpOnly Cookie
SameSite
CSRF Protection
Password hashing
Rate limiting
Input validation
SQL parameterization
Security headers
Midtrans signature verification
```

Password:

```text
bcrypt
atau
argon2id
```

---

# 25. Performance Rules

Karena target utama HG680P:

AI harus mengikuti aturan:

```text
1. Hindari dependency besar.

2. Hindari background worker kecuali benar-benar dibutuhkan.

3. Jangan menjalankan database di application server.

4. Jangan menyimpan image besar secara lokal.

5. Gunakan pagination.

6. Jangan SELECT * untuk data besar.

7. Gunakan index PostgreSQL.

8. Hindari N+1 query.

9. Gunakan connection pool kecil.

10. Gunakan server-rendered HTML.

11. Gunakan HTMX hanya untuk interaksi yang membutuhkan update partial.

12. Hindari SPA framework.
```

---

# 26. PostgreSQL Connection Pool

Karena server HG680P kecil:

Contoh konfigurasi awal:

```text
MaxConnections: 10
MinConnections: 1
```

Nilai final harus disesuaikan hasil load testing.

---

# 27. Deployment

Production:

```text
Internet
 ↓
Cloudflare
 ↓
Nginx
 ↓
Go :8080
 ↓
PostgreSQL Remote
```

Go dijalankan menggunakan:

```text
systemd
```

Tidak perlu:

```text
Docker
PM2
Node.js
PHP-FPM
```

---

# 28. Binary

Build ARM64:

```bash
GOOS=linux GOARCH=arm64 go build -o ecommerce ./cmd/web
```

Production cukup menjalankan:

```bash
./ecommerce
```

---

# 29. Environment

`.env.example`

```text
APP_ENV=
APP_PORT=

DATABASE_URL=

SESSION_SECRET=

GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=

MIDTRANS_SERVER_KEY=
MIDTRANS_CLIENT_KEY=

CLOUDINARY_CLOUD_NAME=
CLOUDINARY_API_KEY=
CLOUDINARY_API_SECRET=

SMTP_HOST=
SMTP_PORT=
SMTP_USER=
SMTP_PASSWORD=
```

Secret tidak boleh dimasukkan ke source code.

---

# 30. Tahapan Development

AI tidak boleh membuat seluruh aplikasi sekaligus.

## Phase 1

```text
Project architecture
Configuration
PostgreSQL
Migration
Base template
Nginx-ready server
```

## Phase 2

```text
Authentication
User
Profile
Address
```

## Phase 3

```text
Category
Product
Image
Attribute
Variant
```

## Phase 4

```text
Cart
```

## Phase 5

```text
Checkout
Order
Shipping
```

## Phase 6

```text
Midtrans
Webhook
Payment
```

## Phase 7

```text
Order history
Reviews
Returns
Refund
```

## Phase 8

```text
Admin
Dashboard
Reports
Settings
```

## Phase 9

```text
Security review
Performance optimization
Load testing
Deployment HG680P
```

---

# 31. Instruksi untuk AI Developer

AI bertindak sebagai:

```text
Senior Go Backend Engineer
+
Server-Side Web Engineer
+
PostgreSQL Engineer
```

Tujuan utama:

```text
Performance
Low Memory Usage
Simple Architecture
Maintainability
Security
```

Sebelum membuat code:

1. Baca seluruh struktur project.
2. Pahami module yang sedang dikerjakan.
3. Jangan membuat duplicate service/repository.
4. Jangan mengubah module lain tanpa kebutuhan.
5. Jangan menambahkan dependency tanpa alasan.
6. Prioritaskan standard library Go.
7. Gunakan modular monolith.
8. Pertahankan satu application binary.

---

# 32. Larangan Arsitektur

AI tidak boleh tanpa persetujuan menggunakan:

```text
Microservices
Next.js
React
Vue
Angular
Node.js runtime production
Prisma
GORM
Redis lokal
RabbitMQ
Kafka
Elasticsearch
Docker Compose production
Kubernetes
```

karena target deployment adalah perangkat rendah resource.

---

# 33. Prinsip Utama

Jika terdapat dua solusi yang menghasilkan fitur sama:

```text
Solution A:
lebih modern tetapi lebih kompleks.

Solution B:
lebih sederhana dan menggunakan resource lebih sedikit.
```

AI harus memilih:

```text
Solution B
```

selama keamanan, maintainability, dan kebutuhan bisnis tetap terpenuhi.

---

# 34. Target Production

Target hardware:

```text
FiberHome HG680P
ARM64
RAM ±2 GB
Linux / Armbian
```

Application server hanya menjalankan:

```text
Linux
Nginx
Go application
systemd
```

Database:

```text
PostgreSQL
Remote Server
```

Static/media:

```text
Cloudinary / CDN
```

Target akhir:

```text
Cloudflare
    ↓
Nginx
    ↓
Go Modular Monolith
    ↓
PostgreSQL Remote
```

Satu server.

Satu aplikasi.

Satu binary.

Resource minimal.

Mudah dikembangkan.

Mudah dipelihara.
