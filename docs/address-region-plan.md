# Rencana kode wilayah alamat (untuk Phase 5 — Shipping)

Status: **ditunda ke Phase 5.** Dokumen ini ditulis di Phase 3 sebagai analisis.

## Kondisi sekarang (Phase 2)

`user_addresses` menyimpan wilayah sebagai teks bebas: `province`, `city`, `district`,
`village`, `postal_code`. Pengguna mengetik sendiri, tanpa daftar pilihan.

## Mengapa kolom kode tidak ditambahkan sekarang

Menambahkan kolom nullable (`province_code`, `city_code`, ...) secara teknis aman, tetapi:

1. Belum ada tabel master wilayah dan form pemilih wilayah, sehingga kolom itu akan selalu
   `NULL`: kolom mati yang memberi kesan data terstruktur padahal tidak.
2. Sistem kode yang dipakai bergantung pada sumber data wilayah dan kebutuhan kurir
   (kode Kemendagri/BPS vs ID area milik penyedia ongkir). Keputusan ini milik Phase 5.
3. Kode Kemendagri bersifat hierarkis (provinsi 2 digit → kab/kota 4 → kecamatan 6/7 →
   kelurahan/desa 10). Cukup **satu** kolom `village_code`; tingkat di atasnya bisa
   diturunkan dari prefiks. Empat kolom kode akan menduplikasi informasi yang sama.

## Rencana migrasi Phase 5

1. **Master wilayah** — migrasi baru, misalnya `00NN_regions.sql`:

   ```sql
   CREATE TABLE regions (
       code        text PRIMARY KEY,           -- kode hierarkis Kemendagri/BPS
       parent_code text REFERENCES regions (code),
       level       smallint NOT NULL CHECK (level BETWEEN 1 AND 4), -- 1 prov … 4 desa
       name        text NOT NULL
   );
   CREATE INDEX regions_parent_idx ON regions (parent_code);
   ```

   Data diimpor sekali lewat subcommand (`ecommerce regions import <folder>`), **tidak**
   di-embed ke binary (±90 ribu baris). Kandidat sumber: dataset IndoArea yang pernah dipakai
   App lama (format kode 2/4/7/10 digit), dengan lisensi dan tanggal pembaruan diverifikasi
   lebih dulu.

2. **Kolom di alamat** — migrasi baru:

   ```sql
   ALTER TABLE user_addresses ADD COLUMN village_code text REFERENCES regions (code);
   ```

   Nullable. Kolom teks tetap dipertahankan sebagai label tampilan dan sebagai snapshot
   alamat di pesanan.

3. **Backfill** — cocokkan teks lama ke `regions` hanya jika hasilnya tunggal (nama desa +
   kecamatan + kota). Yang ambigu dibiarkan `NULL`; pengguna diminta memilih ulang wilayah
   saat checkout atau saat mengubah alamat.

4. **UX** — form alamat menjadi pemilih bertingkat Provinsi → Kab/Kota → Kecamatan →
   Kelurahan memakai HTMX (`hx-get` ke endpoint opsi wilayah). Tanpa JavaScript tetap bisa
   lewat select bertahap. Nilai teks terisi otomatis dari `regions`.

5. **Kurir/ongkir** — ID area penyedia ongkir tidak disimpan di alamat. Gunakan tabel
   pemetaan terpisah, misalnya `shipping_area_map (region_code, provider, provider_area_id)`,
   agar ganti penyedia tidak mengubah data alamat.

## Risiko yang perlu diputuskan di Phase 5

- Lisensi dan kemutakhiran dataset wilayah (pemekaran wilayah terjadi berkala).
- Kecocokan kode pos dengan kelurahan tidak selalu satu-satu; kode pos tetap diisi pengguna.
- Penyedia ongkir yang dipilih menentukan apakah pemetaan cukup di tingkat kecamatan
  atau perlu tingkat kelurahan.
