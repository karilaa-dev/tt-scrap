package core

import "testing"

func TestInstagramConfiguration(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"TT_SCRAP_API_KEY": "test-key-sixteen-characters", "DB_URL": "postgresql://localhost/test"}
	}
	cfg, err := ParseConfig(base())
	if err != nil || cfg.InstagramActiveAccounts != 10 || cfg.InstagramMethod != "app" || cfg.InstagramLegacyFallback || cfg.InstagramAccountConcurrency != 1 || cfg.InstagramAccountDownloadConcurrency != 1 {
		t.Fatal("unexpected Instagram defaults", err)
	}
	env := base()
	env["INSTAGRAM_ACTIVE_ACCOUNTS"] = "7"
	env["INSTAGRAM_METHOD"] = " LEGACY "
	env["INSTAGRAM_LEGACY_FALLBACK"] = "true"
	env["INSTAGRAM_ACCOUNTS_FILE"] = "/private/accounts.json"
	env["INSTAGRAM_ACCOUNT_CONCURRENCY"] = "2"
	env["INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY"] = "3"
	cfg, err = ParseConfig(env)
	if err != nil || cfg.InstagramActiveAccounts != 7 || cfg.InstagramMethod != "legacy" || !cfg.InstagramLegacyFallback || cfg.InstagramAccountConcurrency != 2 || cfg.InstagramAccountDownloadConcurrency != 3 || cfg.InstagramAccountsFile != env["INSTAGRAM_ACCOUNTS_FILE"] {
		t.Fatal("Instagram options not parsed", err)
	}
	for key, value := range map[string]string{"INSTAGRAM_ACTIVE_ACCOUNTS": "0", "INSTAGRAM_METHOD": "rapidapi", "INSTAGRAM_LEGACY_FALLBACK": "maybe", "INSTAGRAM_ACCOUNT_CONCURRENCY": "0", "INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY": "0", "INSTAGRAM_APP_TIMEOUT_SECONDS": "0"} {
		env := base()
		env[key] = value
		if _, err := ParseConfig(env); err == nil || err.Error() != "invalid configuration: "+key {
			t.Errorf("invalid %s accepted: %v", key, err)
		}
	}
}
