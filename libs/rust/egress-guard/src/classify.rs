//! Address classification, line for line with `addrcheck.Classify`.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

/// The verdict on one address.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Class {
    /// Reachable on the public internet: admitted.
    Public,
    /// Loopback, RFC 1918, ULA, CGNAT, benchmarking, site-local: refused
    /// unless an allowlist entry names it.
    Private,
    /// Never dialled, whatever an allowlist says: unspecified, link-local
    /// (the cloud metadata endpoints), multicast, reserved and the named
    /// metadata addresses.
    Forbidden,
}

impl Class {
    /// The spelling the shared vector file and the Go `Class.String` use.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Public => "public",
            Self::Private => "private",
            Self::Forbidden => "forbidden",
        }
    }
}

/// An IPv4 block as (network, prefix length).
type V4Block = (u32, u32);
/// An IPv6 block as (network, prefix length).
type V6Block = (u128, u32);

const V4_FORBIDDEN: &[V4Block] = &[
    (0x0000_0000, 8),  // 0.0.0.0/8: "this network"; reaches the local host on Linux
    (0xc000_0000, 24), // 192.0.0.0/24: IETF protocol assignments
    (0xf000_0000, 4),  // 240.0.0.0/4: reserved, 255.255.255.255 included
    (0x6464_64c8, 32), // 100.100.100.200: Alibaba Cloud metadata, inside CGNAT
    (0xa83f_8110, 32), // 168.63.129.16: Azure platform endpoint
    (0xa9fe_0000, 16), // 169.254.0.0/16: link-local, the metadata endpoints
    (0xe000_0000, 4),  // 224.0.0.0/4: multicast
];

const V4_PRIVATE: &[V4Block] = &[
    (0x7f00_0000, 8),  // loopback
    (0x0a00_0000, 8),  // RFC 1918
    (0xac10_0000, 12), // RFC 1918
    (0xc0a8_0000, 16), // RFC 1918
    (0x6440_0000, 10), // 100.64.0.0/10: CGNAT
    (0xc612_0000, 15), // 198.18.0.0/15: benchmarking
];

const V6_FORBIDDEN: &[V6Block] = &[
    (0xfd00_0ec2_0000_0000_0000_0000_0000_0254, 128), // AWS metadata over IPv6, inside ULA
    (0x0064_ff9b_0001_0000_0000_0000_0000_0000, 48),  // local-use NAT64: embedding not fixed
    (0x0100_0000_0000_0000_0000_0000_0000_0000, 64),  // discard-only
    (0x2001_0000_0000_0000_0000_0000_0000_0000, 32),  // Teredo: the IPv4 is obfuscated
    (0, 96), // IPv4-compatible (deprecated); `::` and `::1` are matched first
    (0xfe80_0000_0000_0000_0000_0000_0000_0000, 10), // link-local
    (0xff00_0000_0000_0000_0000_0000_0000_0000, 8), // multicast
];

const V6_PRIVATE: &[V6Block] = &[
    (0xfc00_0000_0000_0000_0000_0000_0000_0000, 7), // unique-local
    (0xfec0_0000_0000_0000_0000_0000_0000_0000, 10), // deprecated site-local
];

const NAT64: V6Block = (0x0064_ff9b_0000_0000_0000_0000_0000_0000, 96);
const SIX_TO_FOUR: V6Block = (0x2002_0000_0000_0000_0000_0000_0000_0000, 16);

const fn v4_contains((network, prefix): V4Block, address: u32) -> bool {
    let mask = if prefix == 0 {
        0
    } else {
        u32::MAX << (32 - prefix)
    };
    address & mask == network
}

const fn v6_contains((network, prefix): V6Block, address: u128) -> bool {
    let mask = if prefix == 0 {
        0
    } else {
        u128::MAX << (128 - prefix)
    };
    address & mask == network
}

/// The IPv4 address an IPv4-mapped, NAT64 (`64:ff9b::/96`) or 6to4
/// (`2002::/16`) address reaches.
fn embedded_ipv4(address: Ipv6Addr) -> Option<Ipv4Addr> {
    if let Some(v4) = address.to_ipv4_mapped() {
        return Some(v4);
    }
    let bits = u128::from(address);
    let octets = address.octets();
    if v6_contains(NAT64, bits) {
        return Some(Ipv4Addr::new(
            octets[12], octets[13], octets[14], octets[15],
        ));
    }
    if v6_contains(SIX_TO_FOUR, bits) {
        return Some(Ipv4Addr::new(octets[2], octets[3], octets[4], octets[5]));
    }
    None
}

/// The address [`classify`] judges: the IPv4 address an embedded form
/// reaches, any other address as itself.
#[must_use]
pub fn canonical(address: IpAddr) -> IpAddr {
    match address {
        IpAddr::V6(v6) => embedded_ipv4(v6).map_or(address, IpAddr::V4),
        IpAddr::V4(_) => address,
    }
}

/// Decide one address. Matches `addrcheck.Classify` on every vector in the
/// shared file.
#[must_use]
pub fn classify(address: IpAddr) -> Class {
    match canonical(address) {
        IpAddr::V4(v4) => classify_v4(v4),
        IpAddr::V6(v6) => classify_v6(v6),
    }
}

fn classify_v4(address: Ipv4Addr) -> Class {
    let bits = u32::from(address);
    if V4_FORBIDDEN.iter().any(|block| v4_contains(*block, bits)) {
        return Class::Forbidden;
    }
    if V4_PRIVATE.iter().any(|block| v4_contains(*block, bits)) {
        return Class::Private;
    }
    Class::Public
}

fn classify_v6(address: Ipv6Addr) -> Class {
    let bits = u128::from(address);
    if address.is_unspecified() {
        return Class::Forbidden;
    }
    if address.is_loopback() {
        return Class::Private;
    }
    if V6_FORBIDDEN.iter().any(|block| v6_contains(*block, bits)) {
        return Class::Forbidden;
    }
    if V6_PRIVATE.iter().any(|block| v6_contains(*block, bits)) {
        return Class::Private;
    }
    Class::Public
}

#[cfg(test)]
mod tests {
    use super::*;

    fn class(text: &str) -> Class {
        classify(text.parse().expect("address"))
    }

    #[test]
    fn refuses_metadata_even_when_embedded() {
        assert_eq!(class("169.254.169.254"), Class::Forbidden);
        assert_eq!(class("::ffff:169.254.169.254"), Class::Forbidden);
        assert_eq!(class("64:ff9b::a9fe:a9fe"), Class::Forbidden);
        assert_eq!(class("2002:a9fe:a9fe::1"), Class::Forbidden);
        assert_eq!(class("fd00:ec2::254"), Class::Forbidden);
    }

    #[test]
    fn canonical_unwraps_embeddings() {
        for (input, want) in [
            ("::ffff:10.0.0.1", "10.0.0.1"),
            ("64:ff9b::a00:1", "10.0.0.1"),
            ("2002:a00:1::1", "10.0.0.1"),
            ("2606:4700::1111", "2606:4700::1111"),
        ] {
            let got = canonical(input.parse().expect("address"));
            assert_eq!(got.to_string(), want, "{input}");
        }
    }
}
