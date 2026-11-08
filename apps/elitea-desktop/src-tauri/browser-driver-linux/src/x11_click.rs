//! The pure parts of the X11 trusted click (no display needed): where on
//! the root window a page point is, and whether the window under the
//! pointer is the pane's.

/// X's "no window".
pub const NO_WINDOW: u32 = 0;

/// How deep the pointer's window chain is followed (an X tree is shallow;
/// a cycle or a hostile server must not loop the main thread).
pub const MAX_DEPTH: usize = 64;

/// Where the page view sits, in GDK's logical pixels, and its scale.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ViewPlace {
    /// The root-window origin of the GDK window the view draws on
    /// (`gdk_window_get_origin`).
    pub origin: (i32, i32),
    /// The view's offset in that window: its allocation when it has no
    /// window of its own, else zero.
    pub offset: (i32, i32),
    /// The view's size (its allocation).
    pub size: (i32, i32),
    /// The window's scale factor: X coordinates are device pixels.
    pub scale: i32,
}

/// The root-window point (device pixels) of `local`, a point of the view in
/// logical pixels (CSS pixels times the zoom). `None` when it is outside
/// the view or off the X coordinate range.
#[must_use]
pub fn root_point(local: (f64, f64), place: ViewPlace) -> Option<(i16, i16)> {
    let (width, height) = place.size;
    // `contains` is false for NaN too.
    let inside = |value: f64, size: i32| (0.0..=f64::from(size)).contains(&value);
    if !inside(local.0, width) || !inside(local.1, height) {
        return None;
    }
    let axis = |local: f64, size: i32, origin: i32, offset: i32| -> Option<i16> {
        let pixel = local.round().clamp(0.0, f64::from(size.max(0)));
        // In range by the clamp above (0..=size, an i32).
        #[allow(clippy::cast_possible_truncation)]
        let pixel = pixel as i64;
        let logical = i64::from(origin) + i64::from(offset) + pixel;
        i16::try_from(logical * i64::from(place.scale.max(1))).ok()
    };
    Some((
        axis(local.0, width, place.origin.0, place.offset.0)?,
        axis(local.1, height, place.origin.1, place.offset.1)?,
    ))
}

/// Whether the pointer is over the pane: `chain` is the windows under the
/// pointer from the root's child down (QueryPointer's `child`, followed),
/// and `toplevel` the pane's toplevel X window. Any other window there
/// (another application's, on top) is not the pane's.
#[must_use]
pub fn over_the_pane(chain: &[u32], toplevel: u32) -> bool {
    toplevel != NO_WINDOW && chain.contains(&toplevel)
}

#[cfg(test)]
mod tests {
    use super::*;

    const PLACE: ViewPlace = ViewPlace {
        origin: (100, 50),
        offset: (0, 0),
        size: (800, 600),
        scale: 1,
    };

    #[test]
    fn a_point_lands_at_the_window_origin_plus_the_point() {
        assert_eq!(root_point((10.4, 20.6), PLACE), Some((110, 71)));
    }

    #[test]
    fn a_windowless_view_adds_its_allocation() {
        let place = ViewPlace {
            offset: (5, 30),
            ..PLACE
        };
        assert_eq!(root_point((0.0, 0.0), place), Some((105, 80)));
    }

    #[test]
    fn the_scale_turns_logical_into_device_pixels() {
        let place = ViewPlace { scale: 2, ..PLACE };
        assert_eq!(root_point((10.0, 20.0), place), Some((220, 140)));
    }

    #[test]
    fn the_edges_are_in_and_outside_is_refused() {
        assert_eq!(root_point((800.0, 600.0), PLACE), Some((900, 650)));
        for outside in [(-1.0, 0.0), (0.0, -0.5), (800.5, 0.0), (0.0, 601.0)] {
            assert_eq!(root_point(outside, PLACE), None, "{outside:?}");
        }
        assert_eq!(root_point((f64::NAN, 0.0), PLACE), None);
    }

    #[test]
    fn off_the_x_range_is_refused() {
        let place = ViewPlace {
            origin: (32_000, 0),
            scale: 2,
            ..PLACE
        };
        assert_eq!(root_point((0.0, 0.0), place), None);
    }

    #[test]
    fn only_the_panes_own_window_chain_counts() {
        // A window manager's frame, then the pane's toplevel.
        assert!(over_the_pane(&[0x40_0001, 0x60_0003], 0x60_0003));
        // The toplevel itself, directly under the root.
        assert!(over_the_pane(&[0x60_0003], 0x60_0003));
        // Another application's window on top.
        assert!(!over_the_pane(&[0x80_0010, 0x80_0011], 0x60_0003));
        // Nothing under the pointer (the bare root), or no toplevel id.
        assert!(!over_the_pane(&[], 0x60_0003));
        assert!(!over_the_pane(&[NO_WINDOW], NO_WINDOW));
    }
}
