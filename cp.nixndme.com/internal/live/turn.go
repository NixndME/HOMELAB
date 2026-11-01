package live

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is what the TURN REST credential scheme (coturn use-auth-secret) specifies
	"encoding/base64"
	"strconv"
	"time"
)

// TURNCredentialTTL is how long a relay login handed to a Live page stays valid. The relay checks
// it again whenever a connection is refreshed, so it must outlast the longest transfer.
const TURNCredentialTTL = 24 * time.Hour

const turnUserSuffix = ":cp"

// TURNCredential returns a short-lived relay login in the TURN REST API format that coturn checks
// with use-auth-secret: username "<unix expiry>:cp", password base64(HMAC-SHA1(secret, username)).
// The relay needs only the shared secret, so no per-user state is kept anywhere.
func TURNCredential(secret string, now time.Time, ttl time.Duration) (username, password string) {
	username = strconv.FormatInt(now.Add(ttl).Unix(), 10) + turnUserSuffix
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username)) // hash.Hash writes never fail
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
