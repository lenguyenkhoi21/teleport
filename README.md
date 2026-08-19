# Teleport v18.10.3 — Keycloak OIDC Patch

A fork of [Gravitational Teleport](https://github.com/gravitational/teleport) v18.10.3 with a patch that enables **OIDC Authentication via Keycloak** on the OSS edition (no Enterprise license required).

## Quick Start

### 1. Prepare Binaries

```bash
# Build (requires Docker only)
chmod +x build.sh
./build.sh --fast

# Copy binaries to system path
sudo cp output/teleport output/tctl output/tsh /usr/local/bin/
sudo chmod +x /usr/local/bin/{teleport,tctl,tsh}

# Verify
teleport version
tctl version
```

### 2. Configure Teleport

```bash
# Create data and config directories
sudo mkdir -p /var/lib/teleport
sudo mkdir -p /etc/teleport

# Generate config
sudo teleport configure -o /etc/teleport/teleport.yaml \
  --cluster-name=teleport.example.com \
  --public-addr=teleport.example.com:443 \
  --cert-file=/etc/teleport/server.crt \
  --key-file=/etc/teleport/server.key
```

Or create `/etc/teleport/teleport.yaml` manually:

```yaml
version: v3
teleport:
  nodename: teleport-server
  data_dir: /var/lib/teleport
  log:
    severity: INFO    # Change to DEBUG for troubleshooting
    output: stderr

auth_service:
  enabled: true
  cluster_name: teleport.example.com
  listen_addr: 0.0.0.0:3025

  # Authentication config — use OIDC as default
  authentication:
    type: oidc
    # connector_name must match metadata.name in the connector YAML (step 4)
    connector_name: keycloak
    second_factor: "off"        # Disable MFA if not needed
    webauthn:
      rp_id: teleport.example.com

proxy_service:
  enabled: true
  web_listen_addr: 0.0.0.0:443
  public_addr: teleport.example.com:443

  # TLS certs (use Let's Encrypt or self-signed)
  https_keypairs:
    - key_file: /etc/teleport/server.key
      cert_file: /etc/teleport/server.crt

  # Or use ACME (Let's Encrypt) for automatic certs:
  # acme:
  #   enabled: true
  #   email: admin@example.com

ssh_service:
  enabled: true
  listen_addr: 0.0.0.0:3022
```

### 3. Configure Keycloak

#### 3.1 Create a Client in Keycloak

1. Go to **Keycloak Admin** → select your Realm → **Clients** → **Create client**
2. Configure:

| Field | Value |
|-------|-------|
| Client type | OpenID Connect |
| Client ID | `teleport` |
| Client authentication | **ON** |
| Valid redirect URIs | `https://teleport.example.com/v1/webapi/oidc/callback` |
| Web origins | `https://teleport.example.com` |

3. Go to the **Credentials** tab → copy the **Client secret**

#### 3.2 Create a Groups Mapper (REQUIRED)

By default, Keycloak **does NOT** include the `groups` claim in the ID token. You must create a mapper:

1. **Client scopes** → **Create client scope**:
   - Name: `groups`, Type: Default, Protocol: OpenID Connect

2. Open the `groups` scope → **Mappers** → **Add mapper** → **By configuration** → **Group Membership**:
   - Name: `groups`
   - Token Claim Name: `groups`
   - Full group path: **OFF**
   - Add to ID token: **ON**
   - Add to access token: **ON**

3. Go back to **Clients** → `teleport` → **Client scopes** → **Add client scope** → add `groups`

#### 3.3 Create Groups and Users

```
Groups:
  ├── admins       → full access
  ├── developers   → basic access
  └── devops       → access + editor

Users:
  ├── alice (admins)
  └── bob (developers)
```

#### 3.4 Verify Keycloak OIDC endpoint

```bash
# Should return JSON with authorization_endpoint, token_endpoint, jwks_uri
curl -s https://keycloak.example.com/realms/YOUR_REALM/.well-known/openid-configuration | jq .
```

### 4. Create the OIDC Connector

Create a file `keycloak-connector.yaml`:

```yaml
kind: oidc
sub_kind: custom_oidc
version: v3
metadata:
  name: keycloak
spec:
  issuer_url: https://keycloak.example.com/realms/YOUR_REALM
  client_id: teleport
  client_secret: "YOUR_CLIENT_SECRET"
  redirect_url:
    - https://teleport.example.com/v1/webapi/oidc/callback
  scope:
    - openid
    - email
    - profile
    - groups
  display: "Login with Keycloak"
  claims_to_roles:
    - claim: groups
      value: admins
      roles: [access, editor, auditor]
    - claim: groups
      value: developers
      roles: [access]
    - claim: groups
      value: devops
      roles: [access, editor]
```

> ⚠️ **`sub_kind: custom_oidc` is REQUIRED** — without it you will get "OIDC is only available in Teleport Enterprise"

### 5. Start Teleport

```bash
# Start in foreground
sudo teleport start -c /etc/teleport/teleport.yaml

# Start in background
sudo teleport start -c /etc/teleport/teleport.yaml &> /var/log/teleport.log &
```

<details>
<summary>📄 Systemd service file</summary>

Create `/etc/systemd/system/teleport.service`:

```ini
[Unit]
Description=Teleport Service
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/teleport start -c /etc/teleport/teleport.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable teleport
sudo systemctl start teleport
```

</details>

### 6. Apply Connector and Verify

```bash
# Apply OIDC connector
tctl create -f keycloak-connector.yaml

# Verify connector was created
tctl get oidc

# Check that referenced roles exist
tctl get roles --format=text
```

### 7. Test Login

#### Web UI

```bash
# Open browser
open https://teleport.example.com
# → Click "Login with Keycloak" → Authenticate on Keycloak → Redirected back to Teleport
```

#### CLI (tsh)

```bash
# Login via Keycloak
tsh login --proxy=teleport.example.com --auth=keycloak

# Check session
tsh status

# List available nodes
tsh ls

# SSH into a node
tsh ssh user@hostname
```

#### SSO dry-run test

```bash
# Test connector without a real login
tctl sso test --connector-name=keycloak --connector-type=oidc
```

### 8. Troubleshooting

#### Enable debug logging

```yaml
# In teleport.yaml
teleport:
  log:
    severity: DEBUG
```

```bash
sudo systemctl restart teleport
journalctl -u teleport -f | grep -i "oidc\|custom"
```

#### Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `OIDC is only available in Teleport Enterprise` | Missing `sub_kind: custom_oidc` | Add `sub_kind: custom_oidc` to the connector YAML |
| `custom OIDC discovery failed` | Teleport cannot reach Keycloak | Check `issuer_url`, DNS resolution, and firewall rules |
| `user has no roles matched` | Groups claim is empty or doesn't match | Check the Keycloak groups mapper (section 3.2) |
| `redirect URI mismatch` | URL mismatch | `redirect_url` in the connector must exactly match Valid Redirect URIs in Keycloak |
| `Failed to verify ID token` | JWKS error or clock drift | Sync NTP between Teleport server and Keycloak |

#### Inspect claims from Keycloak

```bash
# Get a token directly from Keycloak to inspect claims
curl -s -X POST \
  https://keycloak.example.com/realms/YOUR_REALM/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=teleport" \
  -d "client_secret=YOUR_SECRET" \
  -d "username=alice" \
  -d "password=alice_password" \
  -d "scope=openid email profile groups" \
  | jq -r '.id_token' \
  | cut -d. -f2 \
  | base64 -d 2>/dev/null \
  | jq .
```

The output must contain `groups`:
```json
{
  "preferred_username": "alice",
  "email": "alice@example.com",
  "groups": ["admins"],
  ...
}
```

If `groups` is **missing**, go back to section 3.2 and create the Groups mapper.

---

## Building from Source

See [BUILD_GUIDE.md](BUILD_GUIDE.md) or:

```bash
chmod +x build.sh
./build.sh --fast          # Fast build, skip web UI
./build.sh                 # Full build with web UI
```

## License

AGPL-3.0 — Original license from [Gravitational Teleport](https://github.com/gravitational/teleport).
