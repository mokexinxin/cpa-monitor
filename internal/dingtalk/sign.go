package dingtalk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
)

const webhookEndpoint = "https://oapi.dingtalk.com/robot/send"

func signature(timestamp int64, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func signedWebhookURL(token, secret string, timestamp int64) string {
	query := make(url.Values, 3)
	query.Set("access_token", token)
	query.Set("timestamp", strconv.FormatInt(timestamp, 10))
	query.Set("sign", signature(timestamp, secret))
	return webhookEndpoint + "?" + query.Encode()
}
