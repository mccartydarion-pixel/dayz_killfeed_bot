package database

// BaseRentGiftSQL lets the server owner gift rent days for a rented base: a
// payment row with price 0, no ledger entry (no Champion Points move) and the
// owner who gave it. The due date and pause rules are unchanged.
const BaseRentGiftSQL = `
ALTER TABLE base_rent_payments ADD COLUMN IF NOT EXISTS gifted_by_user_id BIGINT;
ALTER TABLE base_rent_payments ADD COLUMN IF NOT EXISTS gift_note TEXT NOT NULL DEFAULT '';
ALTER TABLE base_rent_payments ALTER COLUMN ledger_entry_id DROP NOT NULL;
ALTER TABLE base_rent_payments DROP CONSTRAINT IF EXISTS base_rent_payments_price_points_check;
ALTER TABLE base_rent_payments DROP CONSTRAINT IF EXISTS base_rent_payments_gift_check;
ALTER TABLE base_rent_payments ADD CONSTRAINT base_rent_payments_gift_check CHECK (
 (gifted_by_user_id IS NULL AND ledger_entry_id IS NOT NULL AND price_points > 0)
 OR (gifted_by_user_id IS NOT NULL AND ledger_entry_id IS NULL AND price_points = 0));
ALTER TABLE base_rent_payments DROP CONSTRAINT IF EXISTS base_rent_payments_gift_note_check;
ALTER TABLE base_rent_payments ADD CONSTRAINT base_rent_payments_gift_note_check CHECK (char_length(gift_note) <= 200);
`
