//! The driver's contract logic over a scripted engine (no WebKit).

use std::sync::Mutex as StdMutex;

use elitea_browser_tools::call::SnapshotId;
use elitea_browser_tools::snapshot::State;

use super::*;

/// An engine that answers from a script of canned replies.
#[derive(Default)]
struct Fake {
    log: StdMutex<Vec<(FrameId, String, Value)>>,
    doc: StdMutex<String>,
    page: StdMutex<Value>,
    frames: Vec<(FrameId, String)>,
    frame_pages: Vec<(FrameId, Value)>,
    click_ignored: bool,
    trusted: bool,
    trusted_clicks: StdMutex<Vec<(f64, f64)>>,
    loads: StdMutex<Vec<String>>,
}

impl Fake {
    fn new(page: Value) -> Self {
        Self {
            doc: StdMutex::new("d1".to_owned()),
            page: StdMutex::new(page),
            ..Self::default()
        }
    }

    fn calls(&self, op: &str) -> Vec<(FrameId, Value)> {
        self.log
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, name, _)| name == op)
            .map(|(frame, _, args)| (*frame, args.clone()))
            .collect()
    }
}

#[async_trait]
impl Engine for Fake {
    async fn call(&self, frame: FrameId, op: &str, args: Value) -> Result<Value, BrowserError> {
        self.log
            .lock()
            .unwrap()
            .push((frame, op.to_owned(), args.clone()));
        let doc = self.doc.lock().unwrap().clone();
        if let Some(expected) = args.get("doc")
            && frame == TOP
            && *expected != json!(doc)
        {
            return Err(BrowserError::stale_ref("gone"));
        }
        Ok(match op {
            "state" => json!({ "doc": doc, "url": "https://a.test/", "title": "A" }),
            "snapshot" if frame == TOP => {
                let mut page = self.page.lock().unwrap().clone();
                page["doc"] = json!(doc);
                page
            }
            "snapshot" => self
                .frame_pages
                .iter()
                .find(|(id, _)| *id == frame)
                .map(|(_, page)| page.clone())
                .ok_or_else(|| BrowserError::new(ErrorCode::Internal, "no frame"))?,
            "click" => json!({
                "ignored": self.click_ignored,
                "rect": { "x": 10.0, "y": 20.0, "width": 40.0, "height": 10.0 }
            }),
            "rect" => json!({ "x": 10.0, "y": 20.0, "width": 40.0, "height": 10.0 }),
            "press" => json!({}),
            _ => json!({}),
        })
    }

    async fn frames(&self) -> Vec<(FrameId, String)> {
        self.frames.clone()
    }

    async fn load(&self, url: &str) -> Result<(), BrowserError> {
        self.loads.lock().unwrap().push(url.to_owned());
        *self.doc.lock().unwrap() = format!("d{}", self.loads.lock().unwrap().len() + 1);
        Ok(())
    }

    async fn history(&self, _step: HistoryStep) -> Result<(), BrowserError> {
        *self.doc.lock().unwrap() = "reloaded".to_owned();
        Ok(())
    }

    async fn page(&self) -> Result<EnginePage, BrowserError> {
        Ok(EnginePage {
            url: "https://a.test/".to_owned(),
            title: "A".to_owned(),
            loading: false,
        })
    }

    async fn screenshot(&self, _rect: Option<RawRect>) -> Result<Screenshot, BrowserError> {
        Ok(Screenshot {
            media_type: "image/png",
            data: vec![1],
            width: 1,
            height: 1,
        })
    }

    fn trusted_input(&self) -> bool {
        self.trusted
    }

    async fn trusted_click(&self, at: (f64, f64), _double: bool) -> Result<(), BrowserError> {
        self.trusted_clicks.lock().unwrap().push(at);
        Ok(())
    }
}

fn allow_all() -> NavigationCheck {
    Arc::new(|_| Ok(()))
}

fn form_page() -> Value {
    json!({
        "url": "https://a.test/", "title": "A", "frames": [],
        "nodes": [
            {"r": "textbox", "n": "Email", "k": 1, "t": "email"},
            {"r": "textbox", "n": "Password", "k": 2, "t": "password", "v": "hunter2"},
            {"r": "button", "n": "Go", "k": 3}
        ]
    })
}

fn driver(engine: Fake) -> WebDriver<Fake> {
    WebDriver::new(engine, allow_all(), Arc::new(NoHandOver))
}

fn latest(index: u32) -> ElementRef {
    ElementRef::latest(index)
}

#[tokio::test]
async fn snapshot_marks_secure_fields_and_keys_elements_by_frame() {
    let driver = driver(Fake::new(form_page()));
    let snapshot = driver.snapshot(100).await.unwrap();
    assert_eq!(
        snapshot.render_tree(),
        "- textbox \"Email\" [ref=e1]\n- textbox \"Password\" [secure] [ref=e2]\n- button \"Go\" [ref=e3]\n"
    );
    assert!(!snapshot.render().contains("hunter2"));
    assert_eq!(snapshot.nodes[2].key, key_of(0, 3));
}

#[tokio::test]
async fn typing_into_a_secure_field_never_reaches_the_page() {
    let driver = driver(Fake::new(form_page()));
    driver.snapshot(100).await.unwrap();
    let refused = driver.type_text(&latest(2), "x", false, false).await;
    assert_eq!(refused.unwrap_err().code(), ErrorCode::SecureFieldRefused);
    assert!(driver.engine().calls("type").is_empty());
    driver
        .type_text(&latest(1), "ada@example.test", false, false)
        .await
        .unwrap();
    let typed = driver.engine().calls("type");
    assert_eq!(typed.len(), 1);
    assert_eq!(typed[0].1["id"], 1);
    assert_eq!(typed[0].1["doc"], "d1");
}

#[tokio::test]
async fn a_new_top_document_makes_every_ref_stale() {
    let driver = driver(Fake::new(form_page()));
    driver.snapshot(100).await.unwrap();
    *driver.engine().doc.lock().unwrap() = "d2".to_owned();
    let stale = driver.click(&latest(3), false).await;
    assert_eq!(stale.unwrap_err().code(), ErrorCode::StaleRef);
    assert!(driver.engine().calls("click").is_empty());
    // The slot is empty now, even after the document is back.
    *driver.engine().doc.lock().unwrap() = "d1".to_owned();
    let stale = driver.click(&latest(3), false).await;
    assert_eq!(stale.unwrap_err().code(), ErrorCode::StaleRef);
}

#[tokio::test]
async fn a_ref_pinned_to_an_older_snapshot_is_stale() {
    let driver = driver(Fake::new(form_page()));
    driver.snapshot(100).await.unwrap();
    driver.snapshot(100).await.unwrap();
    let old = ElementRef::pinned(SnapshotId::sequence(1), 3);
    assert_eq!(
        driver.click(&old, false).await.unwrap_err().code(),
        ErrorCode::StaleRef
    );
}

#[tokio::test]
async fn acts_are_rate_limited() {
    let driver = driver(Fake::new(form_page()));
    driver.snapshot(100).await.unwrap();
    for _ in 0..10 {
        driver
            .scroll(&ScrollTarget::Page {
                direction: Direction::Down,
                pixels: Some(10),
            })
            .await
            .unwrap();
    }
    let limited = driver
        .scroll(&ScrollTarget::Page {
            direction: Direction::Down,
            pixels: Some(10),
        })
        .await;
    // Each scroll settles for ~50 ms, so ten take ~0.5 s: the eleventh is
    // within the second.
    assert_eq!(limited.unwrap_err().code(), ErrorCode::RateLimited);
}

#[tokio::test]
async fn an_ignored_click_needs_the_grant_or_the_person() {
    let mut engine = Fake::new(form_page());
    engine.click_ignored = true;
    let driver = driver(engine);
    driver.snapshot(100).await.unwrap();
    let refused = driver.click(&latest(3), false).await.unwrap_err();
    assert_eq!(refused.code(), ErrorCode::Unsupported);
    assert!(refused.message().contains("browser_request_user"));
    assert!(driver.engine().trusted_clicks.lock().unwrap().is_empty());

    let mut engine = Fake::new(form_page());
    engine.click_ignored = true;
    engine.trusted = true;
    let driver = driver_with(engine);
    driver.snapshot(100).await.unwrap();
    driver.click(&latest(3), false).await.unwrap();
    assert_eq!(
        *driver.engine().trusted_clicks.lock().unwrap(),
        [(30.0, 25.0)]
    );
}

fn driver_with(engine: Fake) -> WebDriver<Fake> {
    driver(engine)
}

#[tokio::test]
async fn open_checks_the_scheme_and_the_host_policy_first() {
    let check: NavigationCheck = Arc::new(|url| {
        if url.contains("blocked") {
            Err(BrowserError::new(ErrorCode::EgressRefused, "not this host"))
        } else {
            Ok(())
        }
    });
    let driver = WebDriver::new(Fake::new(form_page()), check, Arc::new(NoHandOver));
    let refused = driver.open("https://blocked.test/").await.unwrap_err();
    assert_eq!(refused.code(), ErrorCode::EgressRefused);
    let scheme = driver.open("file:///etc/passwd").await.unwrap_err();
    assert_eq!(scheme.code(), ErrorCode::SchemeRefused);
    assert!(driver.engine().loads.lock().unwrap().is_empty());
    let opened = driver.open("https://a.test/").await.unwrap();
    assert!(opened.refs_stale);
    assert_eq!(*driver.engine().loads.lock().unwrap(), ["https://a.test/"]);
}

#[tokio::test]
async fn a_cross_origin_frame_is_read_in_its_own_frame_and_inlined() {
    let mut engine = Fake::new(json!({
        "url": "https://a.test/", "title": "A", "frames": [7],
        "nodes": [
            {"r": "iframe", "n": "Pay", "k": 7, "x": "https://pay.test/widget"},
            {"r": "link", "n": "After", "k": 8}
        ]
    }));
    engine.frames = vec![
        (3, "https://other.test/".to_owned()),
        (4, "https://pay.test/widget".to_owned()),
    ];
    engine.frame_pages = vec![(
        4,
        json!({
            "doc": "f1", "url": "https://pay.test/widget", "title": "", "frames": [],
            "nodes": [{"r": "textbox", "n": "Card", "k": 1, "ac": "cc-number", "v": "4111"},
                      {"r": "button", "n": "Pay", "k": 2}]
        }),
    )];
    let driver = driver(engine);
    let snapshot = driver.snapshot(100).await.unwrap();
    assert_eq!(
        snapshot.render_tree(),
        "- iframe \"Pay\":\n  - textbox \"Card\" [secure] [ref=e1]\n  - button \"Pay\" [ref=e2]\n- link \"After\" [ref=e3]\n"
    );
    driver.click(&latest(2), false).await.unwrap();
    let clicks = driver.engine().calls("click");
    assert_eq!(clicks[0].0, 4, "the click runs in the frame");
    assert_eq!(clicks[0].1["id"], 2);
    assert_eq!(clicks[0].1["doc"], "f1");
    assert_eq!(snapshot.nodes[0].children[1].key, key_of(1, 2));
}

#[test]
fn frames_are_matched_by_address_then_origin() {
    let announced = vec![
        (1, "https://a.test/x".to_owned()),
        (2, "https://b.test/y?z".to_owned()),
        (3, "https://b.test/other".to_owned()),
    ];
    assert_eq!(pick_frame("https://b.test/y?z", &announced, &[]), Some(2));
    assert_eq!(
        pick_frame("https://b.test/redirected", &announced, &[2]),
        Some(3)
    );
    assert_eq!(pick_frame("https://c.test/", &announced, &[]), None);
    assert_eq!(pick_frame("", &announced, &[]), None);
    assert_eq!(
        origin_of("HTTPS://h.test:8443/p"),
        Some("https://h.test:8443".to_owned())
    );
}

#[tokio::test]
async fn request_user_waits_for_the_hand_over() {
    let driver = driver(Fake::new(form_page()));
    let unavailable = driver.request_user("sign in").await.unwrap_err();
    assert_eq!(unavailable.code(), ErrorCode::Unsupported);
}

#[tokio::test]
async fn press_goes_to_the_focused_cross_origin_frame() {
    struct InFrame(Fake);
    #[async_trait]
    impl Engine for InFrame {
        async fn call(&self, frame: FrameId, op: &str, args: Value) -> Result<Value, BrowserError> {
            let answer = self.0.call(frame, op, args).await?;
            Ok(match (frame, op) {
                (TOP, "press") => json!({ "in_frame": true }),
                (5, "focused") => json!(true),
                (_, "focused") => json!(false),
                _ => answer,
            })
        }
        async fn frames(&self) -> Vec<(FrameId, String)> {
            vec![
                (4, "https://x.test/".to_owned()),
                (5, "https://y.test/".to_owned()),
            ]
        }
        async fn load(&self, url: &str) -> Result<(), BrowserError> {
            self.0.load(url).await
        }
        async fn history(&self, step: HistoryStep) -> Result<(), BrowserError> {
            self.0.history(step).await
        }
        async fn page(&self) -> Result<EnginePage, BrowserError> {
            self.0.page().await
        }
        async fn screenshot(&self, rect: Option<RawRect>) -> Result<Screenshot, BrowserError> {
            self.0.screenshot(rect).await
        }
        fn trusted_input(&self) -> bool {
            false
        }
        async fn trusted_click(&self, at: (f64, f64), double: bool) -> Result<(), BrowserError> {
            self.0.trusted_click(at, double).await
        }
    }
    let driver = WebDriver::new(
        InFrame(Fake::new(form_page())),
        allow_all(),
        Arc::new(NoHandOver),
    );
    driver.press(&Key::parse("Enter").unwrap()).await.unwrap();
    let presses = driver.engine().0.calls("press");
    assert_eq!(
        presses.iter().map(|(frame, _)| *frame).collect::<Vec<_>>(),
        [TOP, 5]
    );
}

#[tokio::test]
async fn states_from_the_walk_reach_the_snapshot() {
    let driver = driver(Fake::new(json!({
        "url": "https://a.test/", "title": "A", "frames": [],
        "nodes": [{"r": "checkbox", "n": "Remember", "k": 1, "s": ["checked", "focused"]}]
    })));
    let snapshot = driver.snapshot(10).await.unwrap();
    assert_eq!(snapshot.nodes[0].states, [State::Checked, State::Focused]);
}
