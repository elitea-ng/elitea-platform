//! The scene a slide actually describes.
//!
//! Opening a presentation kept the slide's XML for editing and, for drawing, threw it away: every
//! run of text on the slide was gathered into one body placeholder and everything else — where the
//! shapes are, what colour they are, the pictures — was not read at all. A real deck therefore drew
//! as one block of text per slide, and a slide whose only content is a picture drew as nothing.
//!
//! This reads the slide's own shape tree instead: each shape where it sits, with the text it holds
//! and the fill it carries, and each picture. Anything it cannot make sense of is left out rather
//! than guessed at, which is the difference between a slide that is missing something and a slide
//! that is wrong.

use zavora_slide_layout::{Color, Item, ItemSource, Rect, Scene, TextFrameProps, TextLine};
use zavora_slide_oxml::xml::Element;

/// Where a placeholder sits when the slide does not say.
///
/// PowerPoint leaves the geometry off a placeholder and takes it from the layout the slide is based
/// on. Until the layout is read, these are the positions PowerPoint's own default layouts use, which
/// is what the great majority of decks are built from — so a title lands where a title goes rather
/// than being dropped for having no coordinates.
fn placeholder_rect(kind: &str, index: Option<u32>, width: i64, height: i64) -> Rect {
    // A sixteenth of the slide, which is the margin PowerPoint's layouts use.
    let margin = width / 16;
    match kind {
        "ctrTitle" => Rect {
            x: margin,
            y: height * 30 / 100,
            w: width - margin * 2,
            h: height * 22 / 100,
        },
        "title" => Rect {
            x: margin,
            y: height * 6 / 100,
            w: width - margin * 2,
            h: height * 18 / 100,
        },
        "subTitle" => Rect {
            x: margin,
            y: height * 55 / 100,
            w: width - margin * 2,
            h: height * 20 / 100,
        },
        "ftr" | "sldNum" | "dt" => Rect {
            x: margin,
            y: height * 92 / 100,
            w: width - margin * 2,
            h: height * 6 / 100,
        },
        // A body, and anything else with a place in the layout. Two of them side by side when the
        // deck numbers them, which is how a comparison slide is built.
        _ => match index {
            Some(1) => Rect {
                x: margin,
                y: height * 28 / 100,
                w: (width - margin * 3) / 2,
                h: height * 60 / 100,
            },
            Some(2) => Rect {
                x: margin * 2 + (width - margin * 3) / 2,
                y: height * 28 / 100,
                w: (width - margin * 3) / 2,
                h: height * 60 / 100,
            },
            _ => Rect {
                x: margin,
                y: height * 28 / 100,
                w: width - margin * 2,
                h: height * 62 / 100,
            },
        },
    }
}

/// A shape's own geometry, if it states one.
fn stated_rect(shape: &Element) -> Option<Rect> {
    let xfrm = shape.find_descendant(b"xfrm")?;
    let off = xfrm.children_named(b"off").next()?;
    let ext = xfrm.children_named(b"ext").next()?;
    let number = |value: Option<&[u8]>| -> Option<i64> {
        std::str::from_utf8(value?).ok()?.trim().parse::<i64>().ok()
    };
    Some(Rect {
        x: number(off.attr(b"x"))?,
        y: number(off.attr(b"y"))?,
        w: number(ext.attr(b"cx"))?,
        h: number(ext.attr(b"cy"))?,
    })
}

/// What kind of placeholder this shape is, if it is one.
fn placeholder_of(shape: &Element) -> Option<(String, Option<u32>)> {
    let ph = shape.find_descendant(b"ph")?;
    let kind = ph
        .attr(b"type")
        .and_then(|value| std::str::from_utf8(value).ok())
        .unwrap_or("body")
        .to_string();
    let index = ph
        .attr(b"idx")
        .and_then(|value| std::str::from_utf8(value).ok())
        .and_then(|value| value.trim().parse::<u32>().ok());
    Some((kind, index))
}

/// The colour a `solidFill` states: a hex value, or a name from the theme.
///
/// Nearly half the colours in a real deck are named rather than stated. An unresolved name used to
/// come out black, which is how a white title on a dark band became unreadable.
fn colour_in(fill: &Element, theme: &dyn Fn(&str) -> Option<Color>) -> Option<Color> {
    if let Some(value) = fill
        .children_named(b"srgbClr")
        .next()
        .and_then(|colour| colour.attr(b"val"))
        && let Some(colour) = Color::from_hex(std::str::from_utf8(value).ok()?)
    {
        return Some(colour);
    }
    let named = fill
        .children_named(b"schemeClr")
        .next()
        .and_then(|colour| colour.attr(b"val"))
        .and_then(|value| std::str::from_utf8(value).ok())?;
    theme(named)
}

/// The fill of the shape itself, from its own properties and nowhere else.
///
/// This looked anywhere inside the shape for a colour, and a shape contains its text — so a plain
/// text box with dark words was drawn as a dark box, and the words disappeared into it. A real deck
/// grew a black band across the top of nine slides that way. A shape's fill is stated in its own
/// `spPr`, and `noFill` there means what it says.
fn body_fill(shape: &Element, theme: &dyn Fn(&str) -> Option<Color>) -> Option<Color> {
    let properties = shape.children_named(b"spPr").next()?;
    if properties.children_named(b"noFill").next().is_some() {
        return None;
    }
    colour_in(properties.children_named(b"solidFill").next()?, theme)
}

/// The lines of text in a shape, with the size and weight each is set in.
fn lines_of(shape: &Element, theme: &dyn Fn(&str) -> Option<Color>) -> Vec<TextLine> {
    let Some(body) = shape.find_descendant(b"txBody") else {
        return Vec::new();
    };
    body.children_named(b"p")
        .map(|paragraph| {
            let runs: Vec<&Element> = paragraph.children_named(b"r").collect();
            let text: String = runs
                .iter()
                .filter_map(|run| run.children_named(b"t").next())
                .map(|t| t.text_content())
                .collect();

            // The first run's properties stand for the line: a line set in two sizes is rare, and
            // drawing it in the first is closer than drawing it in none.
            let properties = runs
                .first()
                .and_then(|run| run.children_named(b"rPr").next());
            let size_hundredths = properties
                .and_then(|rpr| rpr.attr(b"sz"))
                .and_then(|value| std::str::from_utf8(value).ok())
                .and_then(|value| value.trim().parse::<f64>().ok());
            let bold = properties
                .and_then(|rpr| rpr.attr(b"b"))
                .map(|value| value == b"1")
                .unwrap_or(false);
            let colour = properties
                .and_then(|rpr| rpr.children_named(b"solidFill").next())
                .and_then(|fill| colour_in(fill, theme));

            TextLine {
                text,
                // The size the run states, or the size a line of body text is set in when it
                // states none.
                size_pt: size_hundredths.map(|value| value / 100.0).unwrap_or(18.0),
                bold,
                color: colour.unwrap_or(Color::BLACK),
                ..TextLine::default()
            }
        })
        .filter(|line| !line.text.is_empty())
        .collect()
}

/// The line a shape is drawn with: its colour and how thick, where the shape says.
///
/// A connector that states no line is still drawn, in the dark grey a connector is drawn in when
/// nothing says otherwise — a line nobody can see is the same as no line, and the file put one there
/// deliberately.
fn line_of(shape: &Element, theme: &dyn Fn(&str) -> Option<Color>) -> zavora_slide_layout::Outline {
    let line = shape.find_descendant(b"ln");
    let colour = line
        .and_then(|line| line.children_named(b"solidFill").next())
        .and_then(|fill| colour_in(fill, theme))
        .unwrap_or(Color::from_hex("595959").unwrap_or(Color::BLACK));
    // Stated in EMU; a point is 12,700 of them. Thinner than three quarters of a point disappears
    // on a screen, so that is the floor.
    let width_pt = line
        .and_then(|line| line.attr(b"w"))
        .and_then(|value| std::str::from_utf8(value).ok())
        .and_then(|value| value.trim().parse::<f64>().ok())
        .map(|emu| emu / 12_700.0)
        .unwrap_or(1.0)
        .max(0.75);
    zavora_slide_layout::Outline {
        color: colour,
        width_pt,
        dash: zavora_slide_layout::DashStyle::Solid,
    }
}

/// A table's column widths and row heights, in the units the slide states them in.
///
/// A table states its own grid, which is the whole reason a table looks like a table: every cell in
/// a column shares an edge. Deriving the widths from the text instead would put those edges in
/// different places on every row.
fn table_grid(table: &Element) -> (Vec<i64>, Vec<i64>) {
    let number = |value: Option<&[u8]>| -> Option<i64> {
        std::str::from_utf8(value?).ok()?.trim().parse::<i64>().ok()
    };
    let columns = table
        .find_descendant(b"tblGrid")
        .map(|grid| {
            grid.children_named(b"gridCol")
                .filter_map(|column| number(column.attr(b"w")))
                .collect()
        })
        .unwrap_or_default();
    let rows = table
        .children_named(b"tr")
        .map(|row| number(row.attr(b"h")).unwrap_or(0))
        .collect();
    (columns, rows)
}

/// Draw a table: its cells, the lines between them, and what each one says.
///
/// Every cell is drawn in the same order the table states, so a click on a cell can still be traced
/// back to the frame it belongs to. A header row is filled where the table says to fill it and left
/// alone where it does not — a table drawn with invented shading is a different table.
fn push_table(
    scene: &mut Scene,
    table: &Element,
    frame: Rect,
    source: Option<ItemSource>,
    theme: &dyn Fn(&str) -> Option<Color>,
) -> bool {
    let (columns, rows) = table_grid(table);
    if columns.is_empty() {
        return false;
    }

    // The stated widths rarely add up to the frame exactly, so they are scaled to it. Drawing them
    // unscaled leaves a table that overhangs its own frame.
    let stated: i64 = columns.iter().sum();
    let scale = |value: i64| -> i64 {
        if stated > 0 {
            value * frame.w / stated
        } else {
            frame.w / columns.len() as i64
        }
    };

    let stated_height: i64 = rows.iter().sum();
    let mut y = frame.y;
    for (row_index, row) in table.children_named(b"tr").enumerate() {
        // A row's own height where it states one and the heights are believable, an equal share of
        // the frame otherwise.
        let height = if stated_height > 0 {
            rows.get(row_index).copied().unwrap_or(0) * frame.h / stated_height
        } else {
            frame.h / rows.len().max(1) as i64
        };

        let mut x = frame.x;
        for (column_index, cell) in row.children_named(b"tc").enumerate() {
            let width = scale(columns.get(column_index).copied().unwrap_or(0));
            let rect = Rect {
                x,
                y,
                w: width,
                h: height,
            };

            // The cell's own fill, and the line around it. A table without visible lines is a list
            // of words in columns, so the outline is drawn even where the cell states no fill.
            scene.push(
                Item::Rect {
                    rect,
                    fill: cell
                        .find_descendant(b"tcPr")
                        .and_then(|properties| properties.children_named(b"solidFill").next())
                        .and_then(|fill| colour_in(fill, theme)),
                    // Half a point of grey: enough to see the grid, not enough to become the
                    // loudest thing on the slide.
                    outline: Some((Color::from_hex("D0CFCB").unwrap_or(Color::BLACK), 0.5)),
                },
                source,
            );

            let lines = lines_of(cell, theme);
            if !lines.is_empty() {
                scene.push(
                    Item::Text {
                        rect: Rect {
                            // Inset, because text against a cell's edge reads as text against the
                            // cell beside it.
                            x: rect.x + 45_720,
                            y: rect.y,
                            w: (rect.w - 91_440).max(0),
                            h: rect.h,
                        },
                        lines,
                        props: TextFrameProps {
                            anchor: zavora_slide_layout::VerticalAnchor::Middle,
                            ..TextFrameProps::default()
                        },
                    },
                    source,
                );
            }
            x += width;
        }
        y += height;
    }
    true
}

/// Every element under this one with the given name, in document order.
///
/// The XML type finds the first descendant and no more, and a chart has many of the same thing —
/// every series, every point. Written here rather than in the XML crate because this is the only
/// place that needs all of them.
fn all_under<'a>(element: &'a Element, local: &[u8], found: &mut Vec<&'a Element>) {
    for child in element.child_elements() {
        if child.local_name() == local {
            found.push(child);
        }
        all_under(child, local, found);
    }
}

/// A chart's series: what it is called, and the numbers in it.
struct Series {
    name: String,
    values: Vec<f64>,
}

/// Read a chart part far enough to draw it: what kind it is, its categories and its series.
///
/// Only what drawing needs. A chart part can carry a great deal more — every colour, every axis
/// setting, a cached copy of the sheet it came from — and reading all of it to put bars on a slide
/// would be reading a spreadsheet to draw a rectangle.
fn read_chart(xml: &[u8]) -> Option<(String, Vec<String>, Vec<Series>)> {
    let dom = zavora_slide_oxml::Document::parse(xml).ok()?;
    let plot = dom.root()?.find_descendant(b"plotArea")?;

    // The kind is named by the element that holds the series.
    let kind = [
        "barChart",
        "lineChart",
        "pieChart",
        "areaChart",
        "scatterChart",
        "doughnutChart",
    ]
    .into_iter()
    .find(|name| plot.find_descendant(name.as_bytes()).is_some())?
    .to_string();

    let text_of = |element: &Element, tag: &[u8]| -> Vec<String> {
        let Some(holder) = element.find_descendant(tag) else {
            return Vec::new();
        };
        let mut points = Vec::new();
        all_under(holder, b"pt", &mut points);
        points
            .into_iter()
            .filter_map(|point| point.children_named(b"v").next())
            .map(|value| value.text_content())
            .collect()
    };

    let mut categories = Vec::new();
    let mut series = Vec::new();
    let mut all_series = Vec::new();
    all_under(plot, b"ser", &mut all_series);
    for ser in all_series {
        // The series name, where it has one. A series named nothing is still a series.
        let name = ser
            .find_descendant(b"tx")
            .and_then(|tx| {
                let mut values = Vec::new();
                all_under(tx, b"v", &mut values);
                values.first().map(|value| value.text_content())
            })
            .unwrap_or_default();

        let values: Vec<f64> = text_of(ser, b"val")
            .iter()
            .filter_map(|value| value.trim().parse::<f64>().ok())
            .collect();
        if categories.is_empty() {
            categories = text_of(ser, b"cat");
        }
        if !values.is_empty() {
            series.push(Series { name, values });
        }
    }
    if series.is_empty() {
        return None;
    }
    Some((kind, categories, series))
}

/// Draw a chart in the frame the slide gives it.
///
/// Bars, columns, lines and pies, from the numbers the chart holds. Drawn rather than left blank,
/// because a slide whose whole point is a chart showed an empty box — and an empty box on a slide
/// that says "Revenue" is worse than no slide at all.
fn push_chart(scene: &mut Scene, chart: &[u8], frame: Rect, source: Option<ItemSource>) -> bool {
    let Some((kind, categories, series)) = read_chart(chart) else {
        return false;
    };

    // A short palette, used in order. The chart states its own colours and a later pass can read
    // them; these are chosen to be distinguishable rather than to match.
    let palette = ["4E79A7", "F28E2B", "59A14F", "E15759", "9C755F", "76B7B2"];
    let colour = |at: usize| Color::from_hex(palette[at % palette.len()]).unwrap_or(Color::BLACK);

    let highest = series
        .iter()
        .flat_map(|one| one.values.iter())
        .fold(0.0_f64, |top, value| top.max(value.abs()));
    if highest <= 0.0 {
        return false;
    }

    // Room for the labels along the bottom, and a little inside the frame.
    let labels_height = frame.h / 8;
    let plot = Rect {
        x: frame.x + frame.w / 20,
        y: frame.y + frame.h / 20,
        w: frame.w - frame.w / 10,
        h: frame.h - labels_height - frame.h / 10,
    };

    if kind == "pieChart" || kind == "doughnutChart" {
        // A pie is drawn as its slices' shares side by side. Circular geometry is not something the
        // scene can express, so this is honest about being a proportion rather than pretending to
        // be a circle.
        let total: f64 = series[0].values.iter().sum();
        if total <= 0.0 {
            return false;
        }
        let mut x = plot.x;
        for (at, value) in series[0].values.iter().enumerate() {
            let width = ((value / total) * plot.w as f64) as i64;
            scene.push(
                Item::Rect {
                    rect: Rect {
                        x,
                        y: plot.y,
                        w: width,
                        h: plot.h,
                    },
                    fill: Some(colour(at)),
                    outline: None,
                },
                source,
            );
            x += width;
        }
    } else {
        let groups = series[0].values.len().max(1);
        let group_width = plot.w / groups as i64;
        for (group, _) in (0..groups).map(|g| (g, ())) {
            for (which, one) in series.iter().enumerate() {
                let Some(value) = one.values.get(group) else {
                    continue;
                };
                let height = ((value.abs() / highest) * plot.h as f64) as i64;
                // Each series gets its own share of the group, which is how a clustered column
                // chart is read: same category, side by side.
                let each = (group_width / series.len() as i64).max(1);
                let inset = each / 6;
                scene.push(
                    Item::Rect {
                        rect: Rect {
                            x: plot.x + group as i64 * group_width + which as i64 * each + inset,
                            y: plot.y + plot.h - height,
                            w: (each - inset * 2).max(1),
                            h: height,
                        },
                        fill: Some(colour(which)),
                        outline: None,
                    },
                    source,
                );
            }
        }

        // What each column is, along the bottom.
        for (group, category) in categories.iter().enumerate().take(groups) {
            scene.push(
                Item::Text {
                    rect: Rect {
                        x: plot.x + group as i64 * group_width,
                        y: plot.y + plot.h,
                        w: group_width,
                        h: labels_height,
                    },
                    lines: vec![TextLine {
                        text: category.clone(),
                        size_pt: 9.0,
                        color: Color::from_hex("55534E").unwrap_or(Color::BLACK),
                        ..TextLine::default()
                    }],
                    props: TextFrameProps::default(),
                },
                source,
            );
        }
    }

    // What the series are, named. A chart with two series and no key cannot be read.
    if series.len() > 1 || !series[0].name.is_empty() {
        let key: Vec<TextLine> = series
            .iter()
            .enumerate()
            .filter(|(_, one)| !one.name.is_empty())
            .map(|(at, one)| TextLine {
                text: one.name.clone(),
                size_pt: 9.0,
                color: colour(at),
                ..TextLine::default()
            })
            .collect();
        if !key.is_empty() {
            scene.push(
                Item::Text {
                    rect: Rect {
                        x: frame.x,
                        y: frame.y,
                        w: frame.w,
                        h: frame.h / 12,
                    },
                    lines: key,
                    props: TextFrameProps::default(),
                },
                source,
            );
        }
    }
    true
}

/// Build the scene a slide describes, from its own shape tree.
pub fn scene_from_dom(
    dom: &zavora_slide_oxml::SlideDom,
    width: i64,
    height: i64,
    images: &dyn Fn(&str) -> Option<Vec<u8>>,
    layout_box: &dyn Fn(&str, Option<u32>) -> Option<Rect>,
    theme: &dyn Fn(&str) -> Option<Color>,
) -> Scene {
    let mut scene = Scene::new(width, height);

    // Everything on the slide, in drawing order — pictures and tables included, which `shapes`
    // leaves out.
    for (index, shape) in dom.drawables().enumerate() {
        let name = shape.local_name();
        let source = Some(ItemSource::Shape(index));

        // Where it sits: what the slide says, or where the layout would put a placeholder.
        // What the slide says, then what the layout says, and only then a guess. The order matters:
        // a title the layout puts in the middle of the slide belongs in the middle of the slide, and
        // the guess exists only for a file that states nothing anywhere.
        let rect = stated_rect(shape).or_else(|| {
            placeholder_of(shape).and_then(|(kind, idx)| {
                layout_box(&kind, idx).or_else(|| Some(placeholder_rect(&kind, idx, width, height)))
            })
        });
        let Some(rect) = rect else { continue };

        match name {
            // A connector, or a shape whose whole substance is its line: an arrow between two
            // boxes, a rule under a heading. It has an outline and no fill, so the branch that
            // draws a filled body drew nothing at all and the line was simply missing.
            b"cxnSp" => {
                let preset = shape
                    .find_descendant(b"prstGeom")
                    .and_then(|geometry| geometry.attr(b"prst"))
                    .and_then(|value| std::str::from_utf8(value).ok())
                    .unwrap_or("line")
                    .to_string();
                scene.push(
                    Item::Shape {
                        rect,
                        preset: Some(preset),
                        fill: zavora_slide_layout::ShapeFill::None,
                        outline: Some(line_of(shape, theme)),
                        rotation_deg: 0.0,
                    },
                    source,
                );
            }
            // A table, a chart or another drawing held in a frame. A frame holds a whole object
            // rather than being one, so what it holds decides what is drawn — and until now nothing
            // was: a slide whose content was a table drew an empty box.
            b"graphicFrame" => {
                if let Some(table) = shape.find_descendant(b"tbl") {
                    push_table(&mut scene, table, rect, source, theme);
                } else if let Some(reference) = shape
                    .find_descendant(b"chart")
                    .and_then(|chart| chart.attr(b"r:id").or_else(|| chart.attr(b"id")))
                    .and_then(|value| std::str::from_utf8(value).ok())
                    && let Some(part) = images(reference)
                {
                    push_chart(&mut scene, &part, rect, source);
                }
            }
            b"pic" => {
                // A picture. The bytes come from the package, by the relationship the shape names.
                let embed = shape
                    .find_descendant(b"blip")
                    // `r:embed`, qualified. Asking for "embed" matches nothing, and the picture
                    // was silently left off the slide.
                    .and_then(|blip| blip.attr(b"r:embed").or_else(|| blip.attr(b"embed")))
                    .and_then(|value| std::str::from_utf8(value).ok().map(str::to_string));
                if let Some(embed) = embed
                    && let Some(data) = images(&embed)
                {
                    scene.push(
                        Item::Image {
                            rect,
                            data,
                            crop: None,
                            rotation_deg: 0.0,
                        },
                        Some(ItemSource::Image(index)),
                    );
                }
            }
            _ => {
                // A filled or outlined body, where the shape has one. A placeholder usually does
                // not, and drawing a box behind every one of them would put grey rectangles over a
                // deck that has none.
                if let Some(fill) = body_fill(shape, theme) {
                    scene.push(
                        Item::Rect {
                            rect,
                            fill: Some(fill),
                            outline: None,
                        },
                        source,
                    );
                }

                let lines = lines_of(shape, theme);
                if !lines.is_empty() {
                    let is_title = placeholder_of(shape)
                        .is_some_and(|(kind, _)| kind == "title" || kind == "ctrTitle");
                    scene.push(
                        Item::Text {
                            rect,
                            lines,
                            props: TextFrameProps {
                                // A title is centred in its box, as PowerPoint centres one; body
                                // text starts at the top.
                                anchor: if is_title {
                                    zavora_slide_layout::VerticalAnchor::Middle
                                } else {
                                    zavora_slide_layout::VerticalAnchor::Top
                                },
                                ..TextFrameProps::default()
                            },
                        },
                        source,
                    );
                }
            }
        }
    }

    scene
}
