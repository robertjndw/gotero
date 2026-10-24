-- +goose Up
-- Email linking for SSO trusts users.email as proof of ownership, so it must
-- know whether gotero ever checked the address. Users can set their own
-- email to anything, which would let them claim someone else's future SSO
-- login.
ALTER TABLE users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;

-- Nothing recorded how existing addresses were set. One that a linked
-- identity provider asserted is the only kind known to be genuine.
UPDATE users u SET email_verified = true
WHERE u.email IS NOT NULL AND EXISTS (
    SELECT 1 FROM user_identities i WHERE i.user_id = u.id AND i.email = u.email
);

-- +goose Down
ALTER TABLE users DROP COLUMN email_verified;
