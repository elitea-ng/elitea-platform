//! Showcase deck exercising the post-parity capabilities: design-system theme,
//! layout patterns, a chart, a table, an autoshape, and the QA layout report.
//!
//! Run: `cargo run -p zavora-slide --example showcase`
//! Writes `~/Downloads/zavora_showcase.pptx` for manual PowerPoint validation.

use zavora_slide::{
    Bullet, ChartKind, ChartSpec, Emu, Layout, LayoutPattern, PatternParams, Presentation,
    apply_design_theme, apply_layout_pattern, qa,
};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut p = Presentation::new();

    // Apply a curated palette + font pairing to the whole deck.
    apply_design_theme(&mut p, "ocean", "modern")?;

    // ── Slide 1: section divider via a layout pattern ──────────────────
    let s0 = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(s0).unwrap();
        apply_layout_pattern(
            &mut slide,
            LayoutPattern::SectionDivider,
            &PatternParams {
                title: Some("Quarterly Business Review".into()),
                items: vec!["FY2026 · Confidential".into()],
                ..Default::default()
            },
        )?;
    }

    // ── Slide 2: title + content with bullets ──────────────────────────
    let s1 = p.add_slide(Layout::TitleContent);
    {
        let mut slide = p.slide_mut(s1).unwrap();
        slide.set_title("Highlights")?;
        slide.add_bullets(&[
            Bullet {
                text: "Revenue up 24% year over year".into(),
                level: 0,
                bold: true,
            },
            Bullet {
                text: "Gross margin expanded to 61%".into(),
                level: 0,
                bold: false,
            },
            Bullet {
                text: "Net retention at 118%".into(),
                level: 1,
                bold: false,
            },
        ])?;
    }

    // ── Slide 3: a clustered-column chart ───────────────────────────────
    let s2 = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(s2).unwrap();
        slide.set_title("Revenue by Quarter").ok();
        let spec = ChartSpec {
            kind: ChartKind::ClusteredColumn,
            categories: vec!["Q1".into(), "Q2".into(), "Q3".into(), "Q4".into()],
            series: vec![
                ("FY25".into(), vec![12.0, 15.0, 18.0, 21.0]),
                ("FY26".into(), vec![16.0, 19.0, 24.0, 29.0]),
            ],
            title: Some("Revenue ($M)".into()),
            legend_position: Some("b".into()),
            data_labels: true,
        };
        slide.add_chart(
            &spec,
            Emu::inches(1.0),
            Emu::inches(1.5),
            Emu::inches(8.0),
            Emu::inches(4.5),
            0,
        )?;
    }

    // ── Slide 4: a table + an autoshape ─────────────────────────────────
    let s3 = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(s3).unwrap();
        let tid = slide.add_table(
            3,
            3,
            Emu::inches(0.5),
            Emu::inches(1.0),
            Emu::inches(6.0),
            Emu::inches(2.5),
        );
        let cells = [
            ["Metric", "FY25", "FY26"],
            ["ARR", "$48M", "$71M"],
            ["Customers", "1,200", "1,850"],
        ];
        for (r, row) in cells.iter().enumerate() {
            for (c, val) in row.iter().enumerate() {
                slide.set_table_cell(tid, r, c, val)?;
            }
        }
    }

    // ── QA: run the deterministic layout report on slide 4 ──────────────
    {
        let slide = p.slide(s3).unwrap();
        let scene = slide.scene();
        let report = qa::analyze_layout(&scene);
        eprintln!(
            "QA slide 4: {} elements, {} findings",
            report.elements.len(),
            report.findings.len()
        );
        for f in &report.findings {
            eprintln!("  - {:?}", f);
        }
    }

    let out = dirs_downloads().join("zavora_showcase.pptx");
    p.save(&out)?;
    println!("wrote {}", out.display());
    Ok(())
}

fn dirs_downloads() -> std::path::PathBuf {
    let home = std::env::var("HOME").unwrap_or_else(|_| ".".into());
    std::path::Path::new(&home).join("Downloads")
}
