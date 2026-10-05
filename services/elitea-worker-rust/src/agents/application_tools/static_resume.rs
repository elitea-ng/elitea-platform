//! Build a graph static selection from immutable ordinary model-call lineage.
use super::*;

pub(crate) async fn install_static_application_resume(
    events: &[Event],
    decisions: Vec<StaticToolDecision>,
    applications: &ApplicationToolPresentationCatalog,
    coordinator: &ApplicationResumeCoordinator,
) -> Result<(), NativeAgentAssemblyError> {
    let (resumes, _) = build_static_application_resumes(events, decisions, applications)?;
    coordinator.install_root(resumes).await
}

pub(crate) async fn prepare_static_application_resume(
    events: &[Event],
    decisions: Vec<StaticToolDecision>,
    applications: &ApplicationToolPresentationCatalog,
    coordinator: &ApplicationResumeCoordinator,
    delegate: Arc<dyn Llm>,
) -> Result<PreparedNestedApplicationResume, NativeAgentAssemblyError> {
    let (resumes, ids) = build_static_application_resumes(events, decisions, applications)?;
    let calls = application_replay_calls(&resumes)?;
    let batch = application_replay_batch(&resumes, &ids)?;
    coordinator.install_root(resumes).await?;
    let user_content = nested_resume_user_content(&ids);
    Ok(PreparedNestedApplicationResume {
        model: Arc::new(ApplicationReplayModel {
            delegate,
            state: AtomicU8::new(REPLAY_APPLICATIONS_PENDING),
            calls,
            batch,
            previous_markers: nested_resume_markers(events)?,
            replay_marker: user_content.clone(),
        }),
        user_content,
        run_config: application_run_config(),
    })
}

fn build_static_application_resumes(
    events: &[Event],
    decisions: Vec<StaticToolDecision>,
    applications: &ApplicationToolPresentationCatalog,
) -> Result<(HashMap<String, ChildApplicationResume>, HashSet<String>), NativeAgentAssemblyError> {
    if decisions.is_empty() || decisions.len() > MAX_PARALLEL_APPLICATION_CALLS {
        return Err(invalid_configuration());
    }
    let mut builders = HashMap::new();
    let mut root_event_id = None;
    let mut submitted = HashSet::new();
    for decision in decisions {
        let mut pause = None;
        for event in events {
            if let Some(candidate) = static_pipeline_tool_pause(event)?
                && candidate.pause_id == decision.pause_id()
                && pause.replace(candidate).is_some()
            {
                return Err(invalid_configuration());
            }
        }
        let pause = pause.ok_or_else(invalid_configuration)?;
        let (original, _, _) = exact_application_call(
            events,
            &pause.container_invocation_id,
            &pause.parent_call_id,
        )?;
        pause.matches_original_call(original)?;
        let boundary = pipeline_application_boundary(events, &pause.event)?;
        let chain = if let Some(boundary) = &boundary {
            pipeline_boundary_call_chain(events, boundary)?
        } else {
            application_call_chain_from(
                events,
                &pause.event.invocation_id,
                &pause.container_invocation_id,
                &pause.parent_call_id,
                &pause.event.branch,
            )?
        };
        let root = chain.last().ok_or_else(invalid_configuration)?;
        bind_application_resume_batch(&mut root_event_id, &root.event_id)?;
        if !submitted.insert(pause.pause_id.clone()) {
            return Err(invalid_configuration());
        }
        if let Some(boundary) = boundary {
            insert_static_scope_request(
                &mut builders,
                &chain,
                events,
                applications,
                boundary,
                decision,
            )?;
        } else {
            let request = Box::new(PipelineStaticToolResume::new(pause, decision)?);
            insert_static_request(&mut builders, &chain, events, applications, request)?;
        }
    }
    let root_event_id = root_event_id.ok_or_else(invalid_configuration)?;
    // This shared bounded scan retains untouched ordinary/dynamic/static/scoped families
    // without invoking their graph or provider, and omits completed sibling effects.
    retain_pending_nested_decisions(
        events,
        &root_event_id,
        &submitted,
        &mut builders,
        applications,
    )?;
    Ok((finish_resume_builders(builders)?, submitted))
}

fn insert_static_request(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    chain: &[ApplicationCallHop],
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    request: Box<PipelineStaticToolResume>,
) -> Result<(), NativeAgentAssemblyError> {
    let mut current = builders;
    let mut catalog = applications;
    for (index, hop) in chain.iter().rev().enumerate() {
        let child_catalog = catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        if completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id) {
            return Err(invalid_configuration());
        }
        let is_pipeline = index + 1 == chain.len();
        let history = if is_pipeline {
            Vec::new()
        } else {
            let task = application_task_with_variables(&hop.arguments)?;
            let mut history = vec![Content::new("user").with_text(task)];
            history.extend(application_resume_history(
                events,
                &hop.owned_invocation_id,
            )?);
            history
        };
        let builder = pipeline_boundary_builder(current, hop, history)?;
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.pipeline_descendants.is_some()
            || builder.pipeline_static.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        if is_pipeline {
            if !builder.children.is_empty() {
                return Err(unsupported_capability());
            }
            builder.pipeline_static = Some(request);
            return Ok(());
        }
        current = &mut builder.children;
        catalog = child_catalog;
    }
    Err(invalid_configuration())
}

fn insert_static_scope_request(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    chain: &[ApplicationCallHop],
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    boundary: PipelineApplicationBoundary,
    decision: StaticToolDecision,
) -> Result<(), NativeAgentAssemblyError> {
    let mut current = builders;
    let mut catalog = applications;
    for (index, hop) in chain.iter().rev().enumerate() {
        let child_catalog = catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        if completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id) {
            return Err(invalid_configuration());
        }
        let pipeline = index + 1 == chain.len();
        let history = if pipeline {
            Vec::new()
        } else {
            let task = application_task_with_variables(&hop.arguments)?;
            let mut value = vec![Content::new("user").with_text(task)];
            value.extend(application_resume_history(
                events,
                &hop.owned_invocation_id,
            )?);
            value
        };
        let builder = pipeline_boundary_builder(current, hop, history)?;
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.pipeline_static.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        if pipeline {
            if !builder.children.is_empty()
                || boundary.scope_events.is_empty()
                || boundary.scope_events.len() > 512
                || serde_json::to_vec(&boundary.scope_events)
                    .map_err(|_| invalid_configuration())?
                    .len()
                    > MAX_RETAINED_PAUSE_BYTES
                || builder
                    .pipeline_boundary_event_id
                    .as_ref()
                    .is_some_and(|id| id != &boundary.pending_event.id)
            {
                return Err(invalid_configuration());
            }
            builder.pipeline_boundary_event_id = Some(boundary.pending_event.id);
            let resume = builder.pipeline_descendants.get_or_insert_with(|| {
                Box::new(PreparedPipelineToolResume {
                    checkpoint: boundary.checkpoint,
                    scopes: Vec::new(),
                    static_scopes: Vec::new(),
                })
            });
            if resume.checkpoint.thread_id() != boundary.checkpoint_thread_id
                || !resume.scopes.is_empty()
            {
                return Err(invalid_configuration());
            }
            if let Some(scope) = resume
                .static_scopes
                .iter_mut()
                .find(|scope| scope.route == boundary.scope_route)
            {
                if serde_json::to_value(&scope.events).map_err(|_| invalid_configuration())?
                    != serde_json::to_value(&boundary.scope_events)
                        .map_err(|_| invalid_configuration())?
                {
                    return Err(invalid_configuration());
                }
                scope.decisions.push(decision);
            } else {
                resume
                    .static_scopes
                    .push(PipelineApplicationScopeStaticDecisions {
                        route: boundary.scope_route,
                        events: boundary.scope_events,
                        decisions: vec![decision],
                    });
            }
            pipeline_resume_scope_ids(resume)?;
            return Ok(());
        }
        if builder.pipeline_descendants.is_some() {
            return Err(unsupported_capability());
        }
        current = &mut builder.children;
        catalog = child_catalog;
    }
    Err(invalid_configuration())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::application_pipeline::static_pause_fixture;

    fn original() -> Event {
        let mut event = Event::with_id("original-static-batch", "original-parent");
        event.branch = APPLICATION_BRANCH_ROOT.to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: [("call-one", "saved_one"), ("call-two", "saved_two")]
                .into_iter()
                .map(|(id, name)| Part::FunctionCall {
                    name: name.to_owned(),
                    args: json!({"task":"work"}),
                    id: Some(id.to_owned()),
                    thought_signature: None,
                })
                .collect(),
        });
        event
    }
    fn catalog() -> ApplicationToolPresentationCatalog {
        let mut value = ApplicationToolPresentationCatalog::default();
        for name in ["saved_one", "saved_two"] {
            value
                .insert(name.to_owned(), name.to_owned(), "pipeline".to_owned())
                .unwrap();
        }
        value
    }

    #[test]
    fn selecting_one_static_pipeline_keeps_an_untouched_parallel_family_immutable() {
        let original = original();
        let (_, _, one, decision) = static_pause_fixture(&original, 0, "before");
        let (_, _, two, _) = static_pause_fixture(&original, 1, "after");
        let stored_two = serde_json::to_vec(&two).unwrap();
        let (resumes, ids) = build_static_application_resumes(
            &[original.clone(), one.clone(), two.clone()],
            vec![decision],
            &catalog(),
        )
        .unwrap();
        assert_eq!(resumes.len(), 2);
        assert_eq!(ids.len(), 1);
        assert!(resumes["call-one"].history.is_empty());
        assert!(matches!(
            &resumes["call-one"].action,
            ChildApplicationResumeAction::PipelineStatic(_)
        ));
        assert!(resumes["call-two"].history.is_empty());
        let ChildApplicationResumeAction::Retained(retained) = &resumes["call-two"].action else {
            panic!("untouched pipeline must be retained");
        };
        assert_eq!(serde_json::to_vec(&retained.events[0]).unwrap(), stored_two);
        let calls = application_replay_calls(&resumes).unwrap();
        assert_eq!(
            calls
                .iter()
                .map(|call| call.call_id.as_str())
                .collect::<Vec<_>>(),
            vec!["call-one", "call-two"]
        );
        let mut projection = Event::new("new-parent");
        projection.provider_metadata.insert(
            APPLICATION_RETAINED_PAUSE_KEY.to_owned(),
            serde_json::to_string(retained).unwrap(),
        );
        let projected = retained_application_events(&projection).unwrap().unwrap();
        let projected_pause = static_pipeline_tool_pause(&projected[0]).unwrap().unwrap();
        assert_eq!(projected_pause.container_invocation_id, "new-parent");
        assert_eq!(projected_pause.event.invocation_id, two.invocation_id);
        assert_eq!(projected_pause.event.id, two.id);
        assert!(
            projected_pause.lineage
                == crate::agents::application_pipeline::PipelineToolCallLineage::from_call(
                    &original, 1
                )
                .unwrap()
        );
        assert_eq!(serde_json::to_vec(&two).unwrap(), stored_two);
    }

    #[test]
    fn second_static_selection_does_not_replay_a_completed_parallel_pipeline() {
        let original = original();
        let (_, _, one, first_decision) = static_pause_fixture(&original, 0, "before");
        let (_, _, two, second_decision) = static_pause_fixture(&original, 1, "after");
        let (first, ids) = build_static_application_resumes(
            &[original.clone(), one.clone(), two.clone()],
            vec![first_decision],
            &catalog(),
        )
        .unwrap();
        let batch = application_replay_batch(&first, &ids).unwrap();
        let mut replay = original.clone();
        replay.id = "replayed-static-batch".to_owned();
        replay.invocation_id = "first-resume".to_owned();
        replay.llm_response.provider_metadata = Some(json!({APPLICATION_REPLAY_BATCH_KEY:batch}));
        let mut completed = Event::with_id("completed-first", "first-resume");
        completed.llm_response.content = Some(Content {
            role: "tool".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: adk_rust::FunctionResponseData::new(
                    "saved_one",
                    json!({"count":1}),
                ),
                id: Some("call-one".to_owned()),
                annotations: None,
            }],
        });
        let (second, ids) = build_static_application_resumes(
            &[original.clone(), one, two, replay, completed],
            vec![second_decision],
            &catalog(),
        )
        .unwrap();
        assert_eq!(ids.len(), 1);
        assert_eq!(second.len(), 1);
        assert!(second.contains_key("call-two"));
        assert_eq!(second["call-two"].ordinal, 2);
        assert_eq!(second["call-two"].batch_event_id, original.id);
        assert_eq!(
            application_replay_calls(&second)
                .unwrap()
                .iter()
                .map(|call| call.call_id.as_str())
                .collect::<Vec<_>>(),
            vec!["call-two"]
        );
    }
    #[tokio::test]
    async fn outer_static_selection_stops_at_saved_graph_and_preserves_the_other_leaf() {
        let (events, ids) =
            crate::agents::application_pipeline::boundary::tests::fixture_with_static(true).await;
        let before = serde_json::to_vec(&events).unwrap();
        let pause = static_pipeline_tool_pause(&events[5]).unwrap().unwrap();
        let meta = serde_json::Map::from_iter([(
            "pipeline_static_tool_resume_v1".to_owned(),
            json!({"revision":1,"decisions":[{"pause_id":pause.pause_id,"child_thread_id":pause.thread_id,"tool_call_id":pause.parent_call_id,"action":"continue","value":"finish"}]}),
        )]);
        let decisions = crate::agents::graph::static_tool_pause::parse_static_tool_decisions(&meta)
            .unwrap()
            .unwrap();
        let mut catalog = ApplicationToolPresentationCatalog::default();
        catalog
            .insert(
                "saved_pipeline".to_owned(),
                "Saved graph".to_owned(),
                "pipeline".to_owned(),
            )
            .unwrap();
        let boundary = pipeline_application_boundary(&events, &events[5])
            .unwrap()
            .unwrap();
        let chain =
            pipeline_boundary_call_chain(&events, &boundary).expect("outer original call chain");
        assert_eq!(chain.len(), 1);
        assert_eq!(chain[0].tool_name, "saved_pipeline");
        let (resumes, selected) =
            build_static_application_resumes(&events, decisions, &catalog).unwrap();
        assert_eq!(selected.len(), 1);
        assert!(ids.contains(selected.iter().next().unwrap()));
        assert_eq!(resumes.len(), 1);
        let resume = &resumes["outer-call"];
        assert!(resume.history.is_empty());
        assert_eq!(resume.batch_event_id, "original-parent-model");
        assert_eq!(resume.ordinal, 1);
        let ChildApplicationResumeAction::PipelineDescendants(prepared) = &resume.action else {
            panic!("saved graph must receive typed scope continuation")
        };
        assert!(prepared.scopes.is_empty());
        assert_eq!(prepared.static_scopes.len(), 1);
        assert_eq!(prepared.static_scopes[0].decisions.len(), 1);
        assert_eq!(prepared.checkpoint.thread_id(), "conversation/outer-call");
        assert_eq!(resume_interrupt_ids(&resumes).unwrap(), selected);
        assert_eq!(serde_json::to_vec(&events).unwrap(), before);
    }
}
