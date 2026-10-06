//! Service-free checks of the production read-before-capacity decision.
use super::*;
use tokio::sync::Semaphore;

fn record(phase: Phase) -> JobRecord {
    JobRecord {
        phase,
        result_json: None,
        failure_code: None,
        runtime_id: Some("retained-runtime".into()),
    }
}

#[tokio::test]
async fn dispatched_hydration_does_not_need_a_second_execution_slot() {
    let capacity = Semaphore::new(1);
    let execution_owner = capacity.try_acquire().unwrap();
    let permit = hydration_capacity(&capacity, async { Ok(record(Phase::Dispatched)) })
        .await
        .unwrap();
    assert!(permit.is_none());
    assert_eq!(capacity.available_permits(), 0);
    drop(execution_owner);
    assert_eq!(capacity.available_permits(), 1);
}

#[tokio::test]
async fn terminal_hydration_keeps_the_existing_no_effect_ready_response() {
    let capacity = Semaphore::new(1);
    let _execution_owner = capacity.try_acquire().unwrap();
    for phase in [
        Phase::Completed,
        Phase::Failed,
        Phase::Cancelled,
        Phase::Uncertain,
    ] {
        assert!(
            hydration_capacity(&capacity, async { Ok(record(phase)) })
                .await
                .unwrap()
                .is_none()
        );
        assert_eq!(capacity.available_permits(), 0);
    }
}

#[tokio::test]
async fn missing_and_reserved_hydration_keep_capacity_ownership() {
    let capacity = Semaphore::new(1);
    let execution_owner = capacity.try_acquire().unwrap();
    for existing in [Err(LedgerError::Missing), Ok(record(Phase::Reserved))] {
        assert!(matches!(
            hydration_capacity(&capacity, async { existing }).await,
            Err(SupervisorError::Busy)
        ));
    }
    drop(execution_owner);
    for existing in [Err(LedgerError::Missing), Ok(record(Phase::Reserved))] {
        let hydration_owner = hydration_capacity(&capacity, async { existing })
            .await
            .unwrap()
            .expect("fresh hydration retains one admission owner");
        assert_eq!(capacity.available_permits(), 0);
        drop(hydration_owner);
        assert_eq!(capacity.available_permits(), 1);
    }
}

#[tokio::test]
async fn identity_storage_and_corrupt_state_errors_precede_capacity_refusal() {
    let capacity = Semaphore::new(1);
    let _execution_owner = capacity.try_acquire().unwrap();
    for error in [
        LedgerError::Conflict,
        LedgerError::Invalid,
        LedgerError::Fenced,
        LedgerError::Database(sqlx::Error::Protocol("fixture storage failure".into())),
        LedgerError::SchemaMigrationRequired(
            crate::sandbox::ledger::SchemaMigrationRequired::MissingColumn,
        ),
    ] {
        let expected = std::mem::discriminant(&error);
        let result = hydration_capacity(&capacity, async { Err(error) }).await;
        let Err(SupervisorError::Ledger(actual)) = result else {
            panic!("a ledger refusal must remain a ledger refusal")
        };
        assert_eq!(std::mem::discriminant(&actual), expected);
        assert_eq!(capacity.available_permits(), 0);
    }
}
