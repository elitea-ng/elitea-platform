//! A reference driver with no engine: the fixture pages as the raw trees a
//! DOM walk would read (wrappers, label text, text runs, secret values left
//! in), and just enough behaviour for the conformance steps. It proves the
//! kit passes against a driver that keeps the contract, and fails against
//! one that does not.

use std::sync::Mutex;
use std::time::Duration;

use async_trait::async_trait;
use elitea_browser_tools::call::{ElementRef, Key, ScrollTarget, WaitCondition, check_url};
use elitea_browser_tools::driver::{
    BrowserDriver, Handback, HistoryStep, PageInfo, PageText, Screenshot,
};
use elitea_browser_tools::snapshot::{Node, Snapshot, SnapshotSlot, State};
use elitea_browser_tools::{BrowserError, ErrorCode, conformance};

const EMAIL: u64 = 1;
const PASSWORD: u64 = 2;
const REMEMBER: u64 = 3;
const COUNTRY: u64 = 4;
const SIGN_IN: u64 = 5;
const TAB_BILLING: u64 = 6;
const MORE: u64 = 7;
const CARD: u64 = 8;
const CSC: u64 = 9;
const NEW_PASSWORD: u64 = 10;
const PAY_PASSWORD: u64 = 11;

#[derive(Default)]
#[allow(clippy::struct_excessive_bools)]
struct Page {
    fixture: Option<&'static conformance::Fixture>,
    email: String,
    remember: bool,
    country: String,
    submitted: bool,
    billing: bool,
    more: bool,
}

pub struct FakeDriver {
    page: Mutex<Page>,
    slot: Mutex<SnapshotSlot>,
    /// A broken driver: it types into secure fields.
    pub ignores_secure_fields: bool,
}

impl FakeDriver {
    pub fn new() -> Self {
        Self {
            page: Mutex::new(Page::default()),
            slot: Mutex::new(SnapshotSlot::new()),
            ignores_secure_fields: false,
        }
    }

    fn info(&self, refs_stale: bool) -> PageInfo {
        let page = self.page.lock().unwrap();
        PageInfo {
            url: page
                .fixture
                .map_or_else(|| "about:blank".to_owned(), url_for),
            title: page.fixture.map(|f| f.title).unwrap_or_default().to_owned(),
            refs_stale,
        }
    }

    /// A real driver admits every act through its pane's `ActRate`; the
    /// kit acts faster than ten a second with no engine, so this one does
    /// not.
    #[allow(clippy::unnecessary_wraps, clippy::unused_self)]
    fn act(&self) -> Result<(), BrowserError> {
        Ok(())
    }

    fn key_of(&self, target: &ElementRef, input: bool) -> Result<u64, BrowserError> {
        let slot = self.slot.lock().unwrap();
        let node = if input && !self.ignores_secure_fields {
            slot.resolve_for_input(target)?
        } else {
            slot.resolve(target)?
        };
        Ok(node.key)
    }
}

pub fn url_for(fixture: &conformance::Fixture) -> String {
    format!("http://127.0.0.1:9/{}.html", fixture.name)
}

fn text(name: &str) -> Node {
    Node::new("text", name)
}

fn labelled(role: &str, name: &str, key: u64) -> Node {
    Node::new(role, name).key(key).children(vec![text(name)])
}

/// The raw tree of the page in its current state.
#[allow(clippy::too_many_lines)]
fn raw_tree(page: &Page) -> Vec<Node> {
    let Some(fixture) = page.fixture else {
        return Vec::new();
    };
    let body = |children| vec![Node::new("generic", "").children(children)];
    match fixture.name {
        "forms" => {
            let mut email = Node::new("textbox", "Email")
                .key(EMAIL)
                .state(State::Required);
            if !page.email.is_empty() {
                email = email.value(&page.email);
            }
            let mut remember = Node::new("checkbox", "Remember me").key(REMEMBER);
            if page.remember {
                remember = remember.state(State::Checked);
            }
            let option = |name: &str| {
                let node = labelled("option", name, 0);
                if page.country == name {
                    node.state(State::Selected)
                } else {
                    node
                }
            };
            let status = if page.submitted {
                "Status: submitted"
            } else {
                "Status: idle"
            };
            body(vec![Node::new("main", "").children(vec![
                    Node::new("heading", "")
                        .state(State::Level(1))
                        .children(vec![text("Sign in")]),
                    Node::new("form", "Sign in").children(vec![
                        text("Email"),
                        email,
                        text("Password"),
                        Node::new("textbox", "Password")
                            .key(PASSWORD)
                            .state(State::Secure)
                            .value("hunter2"),
                        Node::new("generic", "").children(vec![remember, text(" Remember  me ")]),
                        text("Country"),
                        Node::new("combobox", "Country")
                            .key(COUNTRY)
                            .value(&page.country)
                            .children(vec![option("Canada"), option("France")]),
                        text("About you"),
                        Node::new("textbox", "About you"),
                        labelled("button", "Sign in", SIGN_IN),
                    ]),
                    Node::new("paragraph", "").children(vec![
                        text("Status:"),
                        text(status.trim_start_matches("Status: ")),
                    ]),
                ])])
        }
        "aria-widgets" => {
            let tab = |name: &str, selected: bool, key: u64| {
                let node = labelled("tab", name, key);
                if selected {
                    node.state(State::Selected)
                } else {
                    node
                }
            };
            let (panel, panel_text) = if page.billing {
                ("Billing", "Billing settings")
            } else {
                ("General", "General settings")
            };
            let mut more = labelled("button", "More options", MORE);
            more = more.state(if page.more {
                State::Expanded
            } else {
                State::Collapsed
            });
            let mut nodes = vec![
                Node::new("heading", "Widgets")
                    .state(State::Level(1))
                    .children(vec![text("Widgets")]),
                Node::new("tablist", "Settings").children(vec![
                    tab("General", !page.billing, 0),
                    tab("Billing", page.billing, TAB_BILLING),
                ]),
                Node::new("tabpanel", panel).children(vec![text(panel_text)]),
                more,
            ];
            if page.more {
                nodes
                    .push(Node::new("generic", "").children(vec![labelled("link", "Advanced", 0)]));
            }
            nodes.extend([
                labelled("button", "Bold", 0).state(State::Pressed),
                Node::new("switch", "Dark mode"),
                Node::new("slider", "Volume").value("3"),
                Node::new("button", "").children(vec![text("Custom action")]),
                labelled("button", "Unavailable", 0).state(State::Disabled),
                Node::new("alert", "").children(vec![text("Saved")]),
            ]);
            body(nodes)
        }
        "iframes" => body(vec![
            labelled("heading", "Frames", 0).state(State::Level(1)),
            Node::new("iframe", "Embedded form").children(body(vec![
                Node::new("generic", "")
                    .children(vec![text("Inner name"), Node::new("textbox", "Inner name")]),
                labelled("button", "Inner submit", 0),
            ])),
            labelled("link", "After the frame", 0),
        ]),
        "shadow-dom" => body(vec![
            labelled("heading", "Shadow", 0).state(State::Level(1)),
            Node::new("generic", "").children(vec![
                labelled("heading", "Card title", 0).state(State::Level(2)),
                labelled("link", "Light link", 0),
                labelled("button", "Shadow action", 0),
            ]),
        ]),
        "secure-fields" => {
            let field = |name: &str, value: &str, key: u64, secure: bool| {
                let node = Node::new("textbox", name).key(key).value(value);
                let node = if secure {
                    node.state(State::Secure)
                } else {
                    node
                };
                vec![text(name), node]
            };
            let mut form = Vec::new();
            form.extend(field("Name on card", "Ada Lovelace", 0, false));
            form.extend(field("Card number", "4111111111111111", CARD, true));
            form.extend(field("Security code", "737", CSC, true));
            form.extend(field("New password", "s3cret-new", NEW_PASSWORD, true));
            form.extend(field("Password", "hunter2", PAY_PASSWORD, true));
            form.push(labelled("button", "Pay now", 0));
            body(vec![
                labelled("heading", "Checkout", 0).state(State::Level(1)),
                Node::new("form", "Payment").children(form),
            ])
        }
        _ => Vec::new(),
    }
}

fn all_names(nodes: &[Node], out: &mut String) {
    for node in nodes {
        if !node.is_secure() {
            out.push_str(&node.name);
            out.push('\n');
        }
        all_names(&node.children, out);
    }
}

#[async_trait]
impl BrowserDriver for FakeDriver {
    async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
        check_url(url)?;
        let fixture = conformance::FIXTURES
            .iter()
            .find(|fixture| url_for(fixture) == url)
            .ok_or_else(|| BrowserError::new(ErrorCode::NavigationFailed, "no such page"))?;
        *self.page.lock().unwrap() = Page {
            fixture: Some(fixture),
            country: "France".to_owned(),
            ..Page::default()
        };
        self.slot.lock().unwrap().invalidate();
        Ok(self.info(true))
    }

    async fn snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError> {
        let info = self.info(false);
        let tree = raw_tree(&self.page.lock().unwrap());
        let mut slot = self.slot.lock().unwrap();
        let snapshot = Snapshot::build(slot.next_id(), &info.url, &info.title, tree, max_nodes);
        slot.install(snapshot.clone());
        Ok(snapshot)
    }

    async fn text(&self, _max_chars: usize) -> Result<PageText, BrowserError> {
        let info = self.info(false);
        let mut text = String::new();
        all_names(&raw_tree(&self.page.lock().unwrap()), &mut text);
        Ok(PageText {
            url: info.url,
            title: info.title,
            text,
        })
    }

    async fn screenshot(&self, target: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
        if let Some(target) = target {
            self.key_of(target, false)?;
        }
        Ok(Screenshot {
            media_type: "image/png",
            data: b"\x89PNG\r\n\x1a\n".to_vec(),
            width: 1,
            height: 1,
        })
    }

    async fn click(&self, target: &ElementRef, _double: bool) -> Result<PageInfo, BrowserError> {
        self.act()?;
        let key = self.key_of(target, false)?;
        let mut page = self.page.lock().unwrap();
        match key {
            REMEMBER => page.remember = !page.remember,
            SIGN_IN => page.submitted = true,
            TAB_BILLING => page.billing = true,
            MORE => page.more = !page.more,
            _ => {}
        }
        drop(page);
        Ok(self.info(false))
    }

    async fn type_text(
        &self,
        target: &ElementRef,
        text: &str,
        clear: bool,
        _submit: bool,
    ) -> Result<PageInfo, BrowserError> {
        self.act()?;
        let key = self.key_of(target, true)?;
        let mut page = self.page.lock().unwrap();
        match key {
            EMAIL => {
                if clear {
                    page.email.clear();
                }
                page.email.push_str(text);
            }
            PASSWORD | CARD | CSC | NEW_PASSWORD | PAY_PASSWORD => {}
            _ => return Err(BrowserError::invalid("not a text field")),
        }
        drop(page);
        Ok(self.info(false))
    }

    async fn select(
        &self,
        target: &ElementRef,
        values: &[String],
    ) -> Result<PageInfo, BrowserError> {
        self.act()?;
        if self.key_of(target, false)? != COUNTRY {
            return Err(BrowserError::invalid("not a select"));
        }
        if let Some(value) = values.first() {
            self.page.lock().unwrap().country.clone_from(value);
        }
        Ok(self.info(false))
    }

    async fn scroll(&self, target: &ScrollTarget) -> Result<PageInfo, BrowserError> {
        self.act()?;
        if let ScrollTarget::Element(target) = target {
            self.key_of(target, false)?;
        }
        Ok(self.info(false))
    }

    async fn press(&self, _key: &Key) -> Result<PageInfo, BrowserError> {
        self.act()?;
        Ok(self.info(false))
    }

    async fn history(&self, step: HistoryStep) -> Result<PageInfo, BrowserError> {
        if step == HistoryStep::Reload {
            let mut page = self.page.lock().unwrap();
            let fixture = page.fixture;
            *page = Page {
                fixture,
                country: "France".to_owned(),
                ..Page::default()
            };
        }
        self.slot.lock().unwrap().invalidate();
        Ok(self.info(true))
    }

    async fn wait(
        &self,
        condition: &WaitCondition,
        _timeout: Duration,
    ) -> Result<PageInfo, BrowserError> {
        let met = match condition {
            WaitCondition::Text(wanted) => {
                let mut names = String::new();
                let tree = raw_tree(&self.page.lock().unwrap());
                let snapshot = Snapshot::build(
                    elitea_browser_tools::SnapshotId::sequence(0),
                    "",
                    "",
                    tree,
                    10_000,
                );
                all_names(&snapshot.nodes, &mut names);
                names.contains(wanted.as_str())
            }
            WaitCondition::Element(target) => self.key_of(target, false).is_ok(),
            WaitCondition::Url(_) | WaitCondition::NetworkIdle => true,
        };
        if met {
            Ok(self.info(false))
        } else {
            Err(BrowserError::new(
                ErrorCode::Timeout,
                "the condition did not hold",
            ))
        }
    }

    async fn request_user(&self, _reason: &str) -> Result<Handback, BrowserError> {
        Err(BrowserError::new(ErrorCode::NoPerson, "nobody to ask"))
    }
}
