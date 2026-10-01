package database

// SecurityGiftSQL lets a server owner gift paid time for a base service: a
// purchase row with price 0, no ledger entry (no Champion Points move) and the
// owner who gave it. Paid-time, expiry and stacking rules are unchanged.
const SecurityGiftSQL = `
ALTER TABLE security_service_purchases ADD COLUMN IF NOT EXISTS gifted_by_user_id BIGINT;
ALTER TABLE security_service_purchases ADD COLUMN IF NOT EXISTS gift_note TEXT NOT NULL DEFAULT '';
ALTER TABLE security_service_purchases ALTER COLUMN ledger_entry_id DROP NOT NULL;
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_price_points_check;
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_gift_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_gift_check CHECK (
 (gifted_by_user_id IS NULL AND ledger_entry_id IS NOT NULL AND price_points > 0)
 OR (gifted_by_user_id IS NOT NULL AND ledger_entry_id IS NULL AND price_points = 0));
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_gift_note_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_gift_note_check CHECK (char_length(gift_note) <= 200);
`
