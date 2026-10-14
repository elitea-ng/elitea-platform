//! Charts as SVG text, rasterised to PNG by `elitea-office-core` (ADR-0033
//! decision 6).
//!
//! The SDK's generated code drew with matplotlib and saved a PNG; this draws
//! the same five kinds from the table directly, with no code and no font but
//! the bundled Liberation Sans. The output is a pure function of its input,
//! so a chart is reproducible byte for byte, and every label that came from
//! the data is XML-escaped.

use std::fmt::Write as _;

use super::table::{DataError, DataResult};

pub(crate) const WIDTH: u32 = 960;
pub(crate) const HEIGHT: u32 = 600;

/// The chart kinds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Kind {
    Bar,
    Line,
    Scatter,
    Pie,
    Histogram,
}

impl Kind {
    pub(crate) const NAMES: [&'static str; 5] = ["bar", "line", "scatter", "pie", "histogram"];

    pub(crate) fn parse(name: &str) -> DataResult<Self> {
        Ok(match name.trim().to_ascii_lowercase().as_str() {
            "bar" | "column" => Self::Bar,
            "line" => Self::Line,
            "scatter" => Self::Scatter,
            "pie" => Self::Pie,
            "histogram" | "hist" => Self::Histogram,
            other => {
                return Err(super::table::argument(format!(
                    "Unknown chart type `{other}`. Use one of: {}.",
                    Self::NAMES.join(", ")
                )));
            }
        })
    }
}

pub(crate) struct Series {
    pub(crate) name: String,
    /// One value per category (bar, line, pie, histogram).
    pub(crate) values: Vec<Option<f64>>,
    /// Points (scatter).
    pub(crate) points: Vec<(f64, f64)>,
}

pub(crate) struct Chart {
    pub(crate) kind: Kind,
    pub(crate) title: String,
    pub(crate) x_label: String,
    pub(crate) y_label: String,
    pub(crate) categories: Vec<String>,
    pub(crate) series: Vec<Series>,
}

const PALETTE: [&str; 8] = [
    "#4e79a7", "#f28e2b", "#59a14f", "#e15759", "#76b7b2", "#edc948", "#b07aa1", "#9c755f",
];
const FONT: &str = "Liberation Sans, sans-serif";

fn esc(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            c if c.is_control() => out.push(' '),
            c => out.push(c),
        }
    }
    out
}

fn short(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        text.to_owned()
    } else {
        let cut: String = text.chars().take(max.saturating_sub(1)).collect();
        format!("{cut}…")
    }
}

/// The number as an axis label.
fn tick_label(v: f64) -> String {
    let a = v.abs();
    if a >= 1e9 {
        format!("{:.1}B", v / 1e9)
    } else if a >= 1e6 {
        format!("{:.1}M", v / 1e6)
    } else if a >= 1e4 {
        format!("{:.0}k", v / 1e3)
    } else if v == v.trunc() || a >= 100.0 {
        format!("{v:.0}")
    } else if a >= 1.0 {
        format!("{v:.1}")
    } else {
        format!("{v:.3}")
    }
}

/// Round axis bounds and a step so the ticks are tidy numbers.
fn nice_axis(min: f64, max: f64) -> (f64, f64, f64) {
    let (min, max) = if (max - min).abs() < f64::EPSILON {
        (min - 1.0, max + 1.0)
    } else {
        (min, max)
    };
    let raw = (max - min) / 5.0;
    let magnitude = 10f64.powf(raw.log10().floor());
    let step = [1.0, 2.0, 2.5, 5.0, 10.0]
        .iter()
        .map(|m| m * magnitude)
        .find(|s| *s >= raw)
        .unwrap_or(10.0 * magnitude);
    (
        (min / step).floor() * step,
        (max / step).ceil() * step,
        step,
    )
}

struct Frame {
    left: f64,
    right: f64,
    top: f64,
    bottom: f64,
}

impl Frame {
    fn width(&self) -> f64 {
        self.right - self.left
    }
    fn height(&self) -> f64 {
        self.bottom - self.top
    }
}

pub(crate) fn render_svg(chart: &Chart) -> DataResult<String> {
    if chart.series.is_empty() {
        return Err(super::table::argument("The chart has no data to draw."));
    }
    let mut svg = String::new();
    let legend = chart.series.len() > 1 && chart.kind != Kind::Pie;
    let _ = write!(
        svg,
        r##"<svg xmlns="http://www.w3.org/2000/svg" width="{WIDTH}" height="{HEIGHT}" viewBox="0 0 {WIDTH} {HEIGHT}"><rect width="{WIDTH}" height="{HEIGHT}" fill="#ffffff"/>"##
    );
    let _ = write!(
        svg,
        r##"<text x="{}" y="30" text-anchor="middle" font-family="{FONT}" font-size="20" font-weight="bold" fill="#222">{}</text>"##,
        f64::from(WIDTH) / 2.0,
        esc(&short(&chart.title, 90))
    );
    let rotate = chart.categories.len() > 10 && chart.kind != Kind::Scatter;
    let frame = Frame {
        left: 80.0,
        right: f64::from(WIDTH) - if legend { 170.0 } else { 30.0 },
        top: 50.0,
        bottom: f64::from(HEIGHT) - if rotate { 130.0 } else { 80.0 },
    };
    match chart.kind {
        Kind::Pie => pie(&mut svg, chart)?,
        Kind::Scatter => scatter(&mut svg, chart, &frame)?,
        Kind::Bar | Kind::Histogram => bars(&mut svg, chart, &frame, rotate)?,
        Kind::Line => lines(&mut svg, chart, &frame, rotate)?,
    }
    if chart.kind != Kind::Pie {
        let _ = write!(
            svg,
            r##"<text x="{}" y="{}" text-anchor="middle" font-family="{FONT}" font-size="14" fill="#444">{}</text>"##,
            (frame.left + frame.right) / 2.0,
            f64::from(HEIGHT) - 12.0,
            esc(&short(&chart.x_label, 60))
        );
        let _ = write!(
            svg,
            r##"<text transform="translate(20 {}) rotate(-90)" text-anchor="middle" font-family="{FONT}" font-size="14" fill="#444">{}</text>"##,
            (frame.top + frame.bottom) / 2.0,
            esc(&short(&chart.y_label, 50))
        );
    }
    if legend {
        for (i, s) in chart.series.iter().take(12).enumerate() {
            let y = 70.0 + 22.0 * i as f64;
            let _ = write!(
                svg,
                r##"<rect x="{}" y="{}" width="12" height="12" fill="{}"/><text x="{}" y="{}" font-family="{FONT}" font-size="13" fill="#333">{}</text>"##,
                frame.right + 16.0,
                y - 10.0,
                PALETTE[i % PALETTE.len()],
                frame.right + 34.0,
                y,
                esc(&short(&s.name, 18))
            );
        }
    }
    svg.push_str("</svg>");
    Ok(svg)
}

fn y_axis(svg: &mut String, frame: &Frame, lo: f64, hi: f64, step: f64) {
    let mut v = lo;
    let mut guard = 0;
    while v <= hi + step * 1e-9 && guard < 50 {
        let y = frame.bottom - (v - lo) / (hi - lo) * frame.height();
        let _ = write!(
            svg,
            r##"<line x1="{}" y1="{y:.1}" x2="{}" y2="{y:.1}" stroke="#e3e3e3"/><text x="{}" y="{:.1}" text-anchor="end" font-family="{FONT}" font-size="12" fill="#555">{}</text>"##,
            frame.left,
            frame.right,
            frame.left - 8.0,
            y + 4.0,
            tick_label(v)
        );
        v += step;
        guard += 1;
    }
    let _ = write!(
        svg,
        r##"<line x1="{0}" y1="{1}" x2="{0}" y2="{2}" stroke="#888"/><line x1="{0}" y1="{2}" x2="{3}" y2="{2}" stroke="#888"/>"##,
        frame.left, frame.top, frame.bottom, frame.right
    );
}

fn category_label(svg: &mut String, frame: &Frame, x: f64, text: &str, rotate: bool) {
    let label = esc(&short(text, if rotate { 16 } else { 14 }));
    if rotate {
        let _ = write!(
            svg,
            r##"<text transform="translate({x:.1} {}) rotate(-45)" text-anchor="end" font-family="{FONT}" font-size="12" fill="#555">{label}</text>"##,
            frame.bottom + 14.0
        );
    } else {
        let _ = write!(
            svg,
            r##"<text x="{x:.1}" y="{}" text-anchor="middle" font-family="{FONT}" font-size="12" fill="#555">{label}</text>"##,
            frame.bottom + 20.0
        );
    }
}

fn value_range(chart: &Chart, from_zero: bool) -> (f64, f64, f64) {
    let mut lo = f64::INFINITY;
    let mut hi = f64::NEG_INFINITY;
    for v in chart.series.iter().flat_map(|s| s.values.iter().flatten()) {
        lo = lo.min(*v);
        hi = hi.max(*v);
    }
    if !lo.is_finite() {
        (lo, hi) = (0.0, 1.0);
    }
    if from_zero {
        lo = lo.min(0.0);
        hi = hi.max(0.0);
    }
    nice_axis(lo, hi)
}

fn bars(svg: &mut String, chart: &Chart, frame: &Frame, rotate: bool) -> DataResult<()> {
    let (lo, hi, step) = value_range(chart, true);
    y_axis(svg, frame, lo, hi, step);
    let n = chart.categories.len().max(1);
    let slot = frame.width() / n as f64;
    let gap = if chart.kind == Kind::Histogram {
        0.0
    } else {
        0.2 * slot
    };
    let group = slot - gap;
    let per = group / chart.series.len() as f64;
    let zero = frame.bottom - (0.0 - lo) / (hi - lo) * frame.height();
    for (c, category) in chart.categories.iter().enumerate() {
        let x0 = frame.left + slot * c as f64 + gap / 2.0;
        for (s, series) in chart.series.iter().enumerate() {
            let Some(Some(v)) = series.values.get(c) else {
                continue;
            };
            let y = frame.bottom - (v - lo) / (hi - lo) * frame.height();
            let (top, height) = if y < zero {
                (y, zero - y)
            } else {
                (zero, y - zero)
            };
            let stroke = if chart.kind == Kind::Histogram {
                r##" stroke="#ffffff" stroke-width="1""##
            } else {
                ""
            };
            let _ = write!(
                svg,
                r#"<rect x="{:.1}" y="{top:.1}" width="{:.1}" height="{:.1}" fill="{}"{stroke}/>"#,
                x0 + per * s as f64,
                per.max(0.5),
                height.max(0.5),
                PALETTE[s % PALETTE.len()]
            );
        }
        // At most about 40 labels so they stay readable.
        if n <= 40 || c % (n / 40 + 1) == 0 {
            category_label(svg, frame, x0 + group / 2.0, category, rotate);
        }
    }
    Ok(())
}

fn lines(svg: &mut String, chart: &Chart, frame: &Frame, rotate: bool) -> DataResult<()> {
    let (lo, hi, step) = value_range(chart, false);
    y_axis(svg, frame, lo, hi, step);
    let n = chart.categories.len();
    let at = |i: usize| {
        if n <= 1 {
            (frame.left + frame.right) / 2.0
        } else {
            frame.left + frame.width() * i as f64 / (n - 1) as f64
        }
    };
    for (s, series) in chart.series.iter().enumerate() {
        let colour = PALETTE[s % PALETTE.len()];
        let mut path = String::new();
        let mut pen_down = false;
        for (i, v) in series.values.iter().enumerate() {
            match v {
                Some(v) => {
                    let y = frame.bottom - (v - lo) / (hi - lo) * frame.height();
                    let _ = write!(
                        path,
                        "{}{:.1} {y:.1} ",
                        if pen_down { "L" } else { "M" },
                        at(i)
                    );
                    pen_down = true;
                    if n <= 60 {
                        let _ = write!(
                            svg,
                            r#"<circle cx="{:.1}" cy="{y:.1}" r="3" fill="{colour}"/>"#,
                            at(i)
                        );
                    }
                }
                None => pen_down = false,
            }
        }
        let _ = write!(
            svg,
            r#"<path d="{path}" fill="none" stroke="{colour}" stroke-width="2"/>"#
        );
    }
    for (i, category) in chart.categories.iter().enumerate() {
        if n <= 40 || i % (n / 40 + 1) == 0 {
            category_label(svg, frame, at(i), category, rotate);
        }
    }
    Ok(())
}

fn scatter(svg: &mut String, chart: &Chart, frame: &Frame) -> DataResult<()> {
    let points = || chart.series.iter().flat_map(|s| s.points.iter());
    let bounds = |f: fn(&(f64, f64)) -> f64| {
        points()
            .map(f)
            .fold((f64::INFINITY, f64::NEG_INFINITY), |(lo, hi), v| {
                (lo.min(v), hi.max(v))
            })
    };
    let (xmin, xmax) = bounds(|p| p.0);
    let (ymin, ymax) = bounds(|p| p.1);
    if !xmin.is_finite() || !ymin.is_finite() {
        return Err(super::table::argument("There are no points to draw."));
    }
    let (xlo, xhi, xstep) = nice_axis(xmin, xmax);
    let (ylo, yhi, ystep) = nice_axis(ymin, ymax);
    y_axis(svg, frame, ylo, yhi, ystep);
    let mut v = xlo;
    let mut guard = 0;
    while v <= xhi + xstep * 1e-9 && guard < 50 {
        let x = frame.left + (v - xlo) / (xhi - xlo) * frame.width();
        let _ = write!(
            svg,
            r##"<text x="{x:.1}" y="{}" text-anchor="middle" font-family="{FONT}" font-size="12" fill="#555">{}</text>"##,
            frame.bottom + 20.0,
            tick_label(v)
        );
        v += xstep;
        guard += 1;
    }
    for (s, series) in chart.series.iter().enumerate() {
        for (px, py) in &series.points {
            let x = frame.left + (px - xlo) / (xhi - xlo) * frame.width();
            let y = frame.bottom - (py - ylo) / (yhi - ylo) * frame.height();
            let _ = write!(
                svg,
                r#"<circle cx="{x:.1}" cy="{y:.1}" r="3.5" fill="{}" fill-opacity="0.65"/>"#,
                PALETTE[s % PALETTE.len()]
            );
        }
    }
    Ok(())
}

fn pie(svg: &mut String, chart: &Chart) -> DataResult<()> {
    let series = &chart.series[0];
    let values: Vec<f64> = series.values.iter().map(|v| v.unwrap_or(0.0)).collect();
    if values.iter().any(|v| *v < 0.0) {
        return Err(super::table::argument(
            "A pie chart needs non-negative values; this column has negative numbers. Use a bar chart.",
        ));
    }
    let total: f64 = values.iter().sum();
    if total <= 0.0 {
        return Err(DataError::Argument(
            "A pie chart needs values that add up to more than zero.".into(),
        ));
    }
    let (cx, cy, r) = (300.0_f64, 320.0_f64, 220.0_f64);
    let mut angle = -std::f64::consts::FRAC_PI_2;
    for (i, v) in values.iter().enumerate() {
        if *v <= 0.0 {
            continue;
        }
        let sweep = v / total * std::f64::consts::TAU;
        let colour = PALETTE[i % PALETTE.len()];
        if sweep >= std::f64::consts::TAU - 1e-9 {
            let _ = write!(
                svg,
                r#"<circle cx="{cx}" cy="{cy}" r="{r}" fill="{colour}"/>"#
            );
        } else {
            let (x1, y1) = (cx + r * angle.cos(), cy + r * angle.sin());
            let end = angle + sweep;
            let (x2, y2) = (cx + r * end.cos(), cy + r * end.sin());
            let large = i32::from(sweep > std::f64::consts::PI);
            let _ = write!(
                svg,
                r##"<path d="M{cx} {cy} L{x1:.1} {y1:.1} A{r} {r} 0 {large} 1 {x2:.1} {y2:.1} Z" fill="{colour}" stroke="#ffffff" stroke-width="1.5"/>"##
            );
        }
        angle += sweep;
    }
    for (i, v) in values.iter().enumerate().take(24) {
        let y = 90.0 + 22.0 * i as f64;
        let label = chart.categories.get(i).map_or("", String::as_str);
        let _ = write!(
            svg,
            r##"<rect x="580" y="{}" width="12" height="12" fill="{}"/><text x="600" y="{y}" font-family="{FONT}" font-size="13" fill="#333">{} ({:.1}%)</text>"##,
            y - 10.0,
            PALETTE[i % PALETTE.len()],
            esc(&short(label, 28)),
            v / total * 100.0
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample(kind: Kind) -> Chart {
        Chart {
            kind,
            title: "Sales <2024> & more".into(),
            x_label: "Region".into(),
            y_label: "Units".into(),
            categories: vec!["north".into(), "south".into(), "east".into()],
            series: vec![
                Series {
                    name: "a".into(),
                    values: vec![Some(3.0), Some(5.0), None],
                    points: vec![(1.0, 2.0), (2.0, 3.5)],
                },
                Series {
                    name: "b".into(),
                    values: vec![Some(1.0), Some(2.0), Some(8.0)],
                    points: vec![],
                },
            ],
        }
    }

    #[test]
    fn every_kind_renders_valid_escaped_svg() {
        for kind in [Kind::Bar, Kind::Line, Kind::Scatter, Kind::Histogram] {
            let svg = render_svg(&sample(kind)).unwrap();
            assert!(svg.starts_with("<svg") && svg.ends_with("</svg>"));
            assert!(svg.contains("Sales &lt;2024&gt; &amp; more"), "{kind:?}");
            assert!(!svg.contains("<2024>"));
        }
        let mut pie_chart = sample(Kind::Pie);
        pie_chart.series.truncate(1);
        pie_chart.series[0].values = vec![Some(3.0), Some(5.0), Some(2.0)];
        let svg = render_svg(&pie_chart).unwrap();
        assert!(svg.contains("50.0%"));
    }

    #[test]
    fn the_same_chart_is_the_same_bytes() {
        assert_eq!(
            render_svg(&sample(Kind::Bar)).unwrap(),
            render_svg(&sample(Kind::Bar)).unwrap()
        );
    }

    #[test]
    fn a_pie_of_negatives_is_refused() {
        let mut c = sample(Kind::Pie);
        c.series.truncate(1);
        c.series[0].values = vec![Some(-1.0), Some(2.0), Some(1.0)];
        assert!(render_svg(&c).is_err());
    }

    #[test]
    fn ticks_are_tidy() {
        let (lo, hi, step) = nice_axis(0.0, 87.0);
        assert_eq!((lo, hi, step), (0.0, 100.0, 20.0));
    }
}
