listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/material/openbao-tls/server-chain.crt"
  tls_key_file  = "/material/openbao-tls/server.key"
}
