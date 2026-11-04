-- Inventory: product_variants.stock is the only stock figure; every change
-- made outside checkout writes an inventory_adjustments row in the same
-- transaction (manual adjustment, return restock, exchange, order restore).

-- name: LockVariantStock :one
SELECT v.id, v.product_id, v.sku, v.stock, v.active
FROM product_variants v
WHERE v.id = @id
FOR UPDATE;

-- name: SetVariantStock :exec
UPDATE product_variants SET stock = @stock WHERE id = @id;

-- GiveVariantStock adds units back (restock); the row must be locked.
-- name: GiveVariantStock :one
UPDATE product_variants SET stock = stock + @quantity::int
WHERE id = @id
RETURNING id, sku, stock;

-- TakeVariantStock removes units only when enough are left: no row means
-- the stock was insufficient, and nothing changed.
-- name: TakeVariantStock :one
UPDATE product_variants SET stock = stock - @quantity::int
WHERE id = @id AND stock >= @quantity::int
RETURNING id, sku, stock;

-- name: AddInventoryAdjustment :exec
INSERT INTO inventory_adjustments (variant_id, sku, type, quantity_delta, stock_before, stock_after,
                                   reason, reference_type, reference_id, admin_user_id)
VALUES (@variant_id, @sku, @type, @quantity_delta, @stock_before, @stock_after,
        @reason, sqlc.narg('reference_type'), sqlc.narg('reference_id'), sqlc.narg('admin_user_id'));

-- name: VariantAdjustments :many
SELECT a.id, a.type, a.quantity_delta, a.stock_before, a.stock_after, a.reason,
       a.reference_type, a.reference_id, a.created_at, coalesce(u.email, '')::text AS admin_email
FROM inventory_adjustments a
LEFT JOIN users u ON u.id = a.admin_user_id
WHERE a.variant_id = @variant_id
ORDER BY a.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: ProductAdjustments :many
SELECT a.id, a.sku, a.type, a.quantity_delta, a.stock_before, a.stock_after, a.reason,
       a.reference_type, a.reference_id, a.created_at, coalesce(u.email, '')::text AS admin_email
FROM inventory_adjustments a
JOIN product_variants v ON v.id = a.variant_id
LEFT JOIN users u ON u.id = a.admin_user_id
WHERE v.product_id = @product_id
ORDER BY a.id DESC
LIMIT @row_limit;

-- StockVariant is one variant with its product, for the stock form.
-- name: StockVariant :one
SELECT v.id, v.sku, v.stock, v.active, v.price, p.id AS product_id, p.name AS product_name, p.active AS product_active,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM product_variants v
JOIN products p ON p.id = v.product_id
WHERE v.id = @id;

-- StockList lists variants for the stock page: filter "" (all), "low"
-- (1..threshold) or "out" (0); low/out count active variants of active
-- products only. q searches product name or SKU.
-- name: StockList :many
SELECT v.id, v.sku, v.stock, v.active, v.price, p.id AS product_id, p.name AS product_name, p.active AS product_active,
       coalesce((SELECT string_agg(av.value, ' / ' ORDER BY a.display_order, a.id)
                 FROM product_variant_values vv
                 JOIN product_attributes a        ON a.id = vv.attribute_id
                 JOIN product_attribute_values av ON av.id = vv.value_id
                 WHERE vv.variant_id = v.id), '')::text AS variant_label
FROM product_variants v
JOIN products p ON p.id = v.product_id
WHERE (@filter::text = ''
       OR (@filter::text = 'low' AND v.active AND p.active AND v.stock BETWEEN 1 AND @threshold::int)
       OR (@filter::text = 'out' AND v.active AND p.active AND v.stock = 0))
  AND (@q::text = '' OR p.name ILIKE '%' || @q::text || '%' OR v.sku ILIKE '%' || @q::text || '%')
ORDER BY v.stock, p.name, v.id
LIMIT @row_limit OFFSET @row_offset;
