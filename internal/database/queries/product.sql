-- Storefront queries only show active products in active categories that
-- have at least one active variant (price IS NOT NULL).

-- ListProducts serves listing, category pages and search in one statement.
-- Every filter is optional (NULL = off). total is the full match count.
-- name: ListProducts :many
SELECT p.id, p.name, p.slug, p.brand,
       p.price, p.price_max, p.original_price, p.in_stock, p.flash_sale,
       p.sold_count, p.rating_average, p.rating_count,
       coalesce((SELECT i.url FROM product_images i
                 WHERE i.product_id = p.id
                 ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url,
       count(*) OVER () AS total
FROM products p
JOIN categories c ON c.id = p.category_id AND c.active
WHERE p.active
  AND p.price IS NOT NULL
  AND (sqlc.narg(category_id)::bigint IS NULL OR p.category_id = sqlc.narg(category_id))
  AND (sqlc.narg(min_price)::bigint IS NULL OR p.price >= sqlc.narg(min_price))
  AND (sqlc.narg(max_price)::bigint IS NULL OR p.price <= sqlc.narg(max_price))
  AND (NOT @flash_sale_only::boolean OR p.flash_sale)
  AND (sqlc.narg(query)::text IS NULL
       OR p.search_vector @@ to_tsquery('simple', sqlc.narg(query))
       OR to_tsvector('simple', c.name) @@ to_tsquery('simple', sqlc.narg(query)))
ORDER BY
  CASE WHEN @sort::text = 'price_asc'   THEN p.price END ASC,
  CASE WHEN @sort::text = 'price_desc'  THEN p.price END DESC,
  CASE WHEN @sort::text = 'bestselling' THEN p.sold_count END DESC,
  CASE WHEN @sort::text = 'relevance'
       THEN ts_rank(p.search_vector, to_tsquery('simple', coalesce(sqlc.narg(query), ''))) END DESC,
  p.created_at DESC, p.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- HomeProducts returns the three home sections in one round trip.
-- name: HomeProducts :many
(SELECT 'flash_sale'::text AS section, p.id, p.name, p.slug, p.brand,
        p.price, p.price_max, p.original_price, p.in_stock, p.flash_sale,
        p.sold_count, p.rating_average, p.rating_count,
        coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                  ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url
 FROM products p JOIN categories c ON c.id = p.category_id AND c.active
 WHERE p.active AND p.price IS NOT NULL AND p.flash_sale
 ORDER BY p.in_stock DESC, p.sold_count DESC, p.id DESC
 LIMIT @section_limit)
UNION ALL
(SELECT 'best_seller'::text, p.id, p.name, p.slug, p.brand,
        p.price, p.price_max, p.original_price, p.in_stock, p.flash_sale,
        p.sold_count, p.rating_average, p.rating_count,
        coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                  ORDER BY i.display_order, i.id LIMIT 1), '')::text
 FROM products p JOIN categories c ON c.id = p.category_id AND c.active
 WHERE p.active AND p.price IS NOT NULL AND p.sold_count > 0
 ORDER BY p.sold_count DESC, p.id DESC
 LIMIT @section_limit)
UNION ALL
(SELECT 'recommended'::text, p.id, p.name, p.slug, p.brand,
        p.price, p.price_max, p.original_price, p.in_stock, p.flash_sale,
        p.sold_count, p.rating_average, p.rating_count,
        coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                  ORDER BY i.display_order, i.id LIMIT 1), '')::text
 FROM products p JOIN categories c ON c.id = p.category_id AND c.active
 WHERE p.active AND p.price IS NOT NULL
 ORDER BY p.featured DESC, p.in_stock DESC, p.created_at DESC, p.id DESC
 LIMIT @recommended_limit);

-- name: ListRelatedProducts :many
SELECT p.id, p.name, p.slug, p.brand,
       p.price, p.price_max, p.original_price, p.in_stock, p.flash_sale,
       p.sold_count, p.rating_average, p.rating_count,
       coalesce((SELECT i.url FROM product_images i WHERE i.product_id = p.id
                 ORDER BY i.display_order, i.id LIMIT 1), '')::text AS image_url
FROM products p
WHERE p.active AND p.price IS NOT NULL
  AND p.category_id = @category_id AND p.id <> @product_id
ORDER BY p.in_stock DESC, p.sold_count DESC, p.id DESC
LIMIT @row_limit;

-- GetProductDetail loads a product with its images, attributes and active
-- variants as JSON in one round trip and one consistent snapshot.
-- name: GetProductDetail :one
SELECT p.id, p.category_id, p.name, p.slug, p.description, p.brand,
       p.featured, p.flash_sale, p.sold_count, p.rating_average, p.rating_count,
       c.name AS category_name, c.slug AS category_slug,
       coalesce((SELECT json_agg(json_build_object('url', i.url, 'alt', i.alt)
                                 ORDER BY i.display_order, i.id)
                 FROM product_images i WHERE i.product_id = p.id), '[]')::json AS images,
       coalesce((SELECT json_agg(json_build_object(
                          'id', a.id, 'name', a.name,
                          'values', (SELECT coalesce(json_agg(json_build_object('id', av.id, 'value', av.value)
                                                              ORDER BY av.display_order, av.id), '[]')
                                     FROM product_attribute_values av WHERE av.attribute_id = a.id))
                        ORDER BY a.display_order, a.id)
                 FROM product_attributes a WHERE a.product_id = p.id), '[]')::json AS attributes,
       coalesce((SELECT json_agg(json_build_object(
                          'id', v.id, 'sku', v.sku, 'price', v.price,
                          'original_price', v.original_price, 'stock', v.stock,
                          'value_ids', (SELECT coalesce(json_agg(vv.value_id ORDER BY vv.attribute_id), '[]')
                                        FROM product_variant_values vv WHERE vv.variant_id = v.id))
                        ORDER BY v.id)
                 FROM product_variants v WHERE v.product_id = p.id AND v.active), '[]')::json AS variants
FROM products p
JOIN categories c ON c.id = p.category_id
WHERE p.slug = @slug AND p.active AND c.active;

-- Admin-ready writes (used by product.Service.Create inside a transaction).

-- name: CreateProduct :one
INSERT INTO products (category_id, name, slug, description, brand, active, featured, flash_sale)
VALUES (@category_id, @name, @slug, @description, @brand, @active, @featured, @flash_sale)
RETURNING id;

-- name: CreateProductImage :exec
INSERT INTO product_images (product_id, url, alt, display_order)
VALUES (@product_id, @url, @alt, @display_order);

-- name: CreateProductAttribute :one
INSERT INTO product_attributes (product_id, name, display_order)
VALUES (@product_id, @name, @display_order)
RETURNING id;

-- name: CreateProductAttributeValue :one
INSERT INTO product_attribute_values (attribute_id, value, display_order)
VALUES (@attribute_id, @value, @display_order)
RETURNING id;

-- name: CreateProductVariant :one
INSERT INTO product_variants (product_id, sku, price, original_price, stock, active)
VALUES (@product_id, @sku, @price, @original_price, @stock, true)
RETURNING id;

-- name: CreateProductVariantValue :exec
INSERT INTO product_variant_values (product_id, variant_id, attribute_id, value_id)
VALUES (@product_id, @variant_id, @attribute_id, @value_id);
