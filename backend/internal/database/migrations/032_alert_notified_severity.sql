-- +goose Up
-- The highest severity an alert has been notified at. An alert that gets
-- worse is announced again at once and drops an acknowledgement given for
-- the lower severity — but only the first time it reaches a severity: one
-- that flaps between warning and critical does not notify at every swing.
-- NULL = not notified yet (also every alert opened before this column
-- existed, whose next escalation is therefore announced). Additive.
ALTER TABLE alerts ADD COLUMN notified_severity TEXT;

-- +goose Down
ALTER TABLE alerts DROP COLUMN notified_severity;
