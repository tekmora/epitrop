<!--
Provenance:
Source URL: https://raw.githubusercontent.com/BerriAI/litellm/d665dc1a6315847fcad6cc2c1264ceb47c06abce/litellm/proxy/management_endpoints/key_management_endpoints.py
Pinned revision: d665dc1a6315847fcad6cc2c1264ceb47c06abce
Full document sha256: c9c644c2a93e20ef9d850454ddb4b23bf1db4823b66be415b9a13a5da045b4de
Extraction date: 2026-10-17
SPDX licence: MIT
Copyright holder: Berri AI
Changes: provenance header added by Epitrop; extracted the /key/info handler docstring verbatim
-->

# LiteLLM GET /key/info

Docstring of the handler, verbatim up to its curl examples:

```text

    Retrieve information about a key.

    Parameters:
    - key: str | None (query parameter) - The key to look up. Accepts the plaintext key or its hash;
      prefer the hash, since a query parameter is recorded verbatim by any HTTP access log in front
      of the proxy. Defaults to the key in the Authorization header.

    Returns:
    - key: str - The key that was looked up, echoed back as it was passed in
    - info: dict - The key's row, minus the hashed token. Deleted keys are served from the
      LiteLLM_DeletedVerificationToken archive and carry deleted_at / deleted_by
        - status: "active" | "expired" | "revoked" | "deleted" - Derived from blocked, expires and
          whether the row came from the archive
        - key_alias: str | None - User-friendly key alias
        - spend: float - Amount spent by the key. When budget_duration is set this covers only the
          current budget window, not the key's lifetime
        - max_budget: float | None - Max budget for the key, enforced against spend
        - budget_duration: str | None - Budget reset period ("30d", "1h", etc.)
        - budget_reset_at: datetime | None - When the current budget window ends and spend is next
          reset to 0, not when it was last reset. Reset times snap to standard boundaries in the
          configured timezone (30d and 1mo land on the 1st of the month, 7d on Monday, 1h on the
          hour), so subtracting budget_duration from it does not give the window's start
        - model_max_budget: dict - Per-model budgets, e.g. {"gpt-4": {"budget_limit": 0.0005, "time_period": "30d"}}
        - model_max_budget_usage: dict | None - Current-window spend per model, present only when
          the key has per-model budgets
        - budget_limits: list | None - Concurrent budget windows, exactly as stored
        - budget_limits_usage: dict | None - Current-window spend per budget window, e.g.
          {"1h": {"current_spend": 0.0009}}, present only when the key has budget windows
          (read from the same cross-pod spend counter the budget enforcement uses)
        - models: list - Model_name's the key is allowed to call
        - tpm_limit / rpm_limit: int | None - Tokens and requests per minute limits
        - metadata: dict - Metadata for the key, e.g. {"team": "core-infra"}
        - blocked: bool | None - Whether the key is blocked
        - expires: datetime | None - When the key stops authenticating requests
        - last_active: datetime | None - When the key was last used
        - object_permission: dict | None - Resolved vector store / MCP permissions when the key has
          an object_permission_id
```
