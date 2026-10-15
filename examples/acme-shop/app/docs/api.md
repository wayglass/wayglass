# API

All responses are JSON.

| Method | Path | Answer |
|---|---|---|
| GET | `/healthz` | `{"status":"ok"}` |
| GET | `/api/products` | `{"products":[...]}` |
| GET | `/api/products/{id}` | One product, or 404. |
| POST | `/api/login` | `{"session":"..."}`, or 401. |
