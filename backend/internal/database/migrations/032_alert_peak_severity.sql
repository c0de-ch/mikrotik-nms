-- +goose Up
-- The highest severity an alert has reached while open. An alert that gets
-- worse is announced again at once and drops an acknowledgement given for
-- the lower severity — but only the first time it reaches a severity: one
-- that swings between warning and critical does not fire at every swing,
-- whether or not a notification could be delivered. Open alerts start at
-- the severity they have now. Additive.
ALTER TABLE alerts ADD COLUMN peak_severity TEXT;
UPDATE alerts SET peak_severity = severity WHERE state <> 'resolved';

-- +goose Down
ALTER TABLE alerts DROP COLUMN peak_severity;
