#![no_main]
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    elitea_office_core::testing::fuzz_deck(data);
});
