# Key-Value Storage Service Specification

## [Stage: Core] Basic Key-Value Storage
- `Put(key, value)`: Stores a key and associated byte payload. Overwrites existing values if the key is already present.
- `Get(key)`: Retrieves the byte payload associated with the key.
- If `Get` is invoked with a key that does not exist, the service returns a `NOT_FOUND` error.
