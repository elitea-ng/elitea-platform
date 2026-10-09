use serde_json::{Map, Value};

use super::client::{
    PostmanClientError, conflict, invalid_input, invalid_response, resource_exhausted,
};

const MAX_TREE_DEPTH: usize = 64;
const MAX_TREE_ITEMS: usize = 4_096;

#[derive(Clone, Copy)]
pub(super) enum TreeKind {
    Folder,
    Request,
}

pub(super) fn resolve_indices(
    collection: &Map<String, Value>,
    path: &str,
    kind: TreeKind,
) -> Result<Option<Vec<usize>>, PostmanClientError> {
    let parts = path
        .split('/')
        .map(str::trim)
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    if parts.is_empty() || parts.len() > MAX_TREE_DEPTH {
        return Err(invalid_input());
    }
    let mut items = collection
        .get("item")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    let mut indexes = Vec::with_capacity(parts.len());
    for (depth, part) in parts.iter().enumerate() {
        let mut found = None;
        for (index, item) in items.iter().enumerate() {
            if item
                .get("name")
                .and_then(Value::as_str)
                .is_some_and(|name| name.eq_ignore_ascii_case(part))
                && found.replace(index).is_some()
            {
                return Err(conflict());
            }
        }
        let Some(index) = found else {
            return Ok(None);
        };
        indexes.push(index);
        let item = items.get(index).ok_or_else(invalid_response)?;
        if depth + 1 == parts.len() {
            let is_kind = match kind {
                TreeKind::Folder => item.get("item").is_some(),
                TreeKind::Request => item.get("request").is_some(),
            };
            return Ok(is_kind.then_some(indexes));
        }
        items = item
            .get("item")
            .and_then(Value::as_array)
            .ok_or_else(invalid_input)?;
    }
    Ok(None)
}

pub(super) fn item_at<'a>(
    collection: &'a Map<String, Value>,
    indexes: &[usize],
) -> Result<&'a Value, PostmanClientError> {
    let items = collection
        .get("item")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    item_at_slice(items, indexes)
}

fn item_at_slice<'a>(
    items: &'a [Value],
    indexes: &[usize],
) -> Result<&'a Value, PostmanClientError> {
    let (index, rest) = indexes.split_first().ok_or_else(invalid_input)?;
    let item = items.get(*index).ok_or_else(invalid_response)?;
    if rest.is_empty() {
        return Ok(item);
    }
    let children = item
        .get("item")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    item_at_slice(children, rest)
}

pub(super) fn remove_at(
    collection: &mut Map<String, Value>,
    indexes: &[usize],
) -> Result<Value, PostmanClientError> {
    let items = collection
        .get_mut("item")
        .and_then(Value::as_array_mut)
        .ok_or_else(invalid_response)?;
    remove_from_slice(items, indexes)
}

fn remove_from_slice(
    items: &mut Vec<Value>,
    indexes: &[usize],
) -> Result<Value, PostmanClientError> {
    let (index, rest) = indexes.split_first().ok_or_else(invalid_input)?;
    if rest.is_empty() {
        if *index >= items.len() {
            return Err(invalid_response());
        }
        return Ok(items.remove(*index));
    }
    let item = items.get_mut(*index).ok_or_else(invalid_response)?;
    let children = item
        .get_mut("item")
        .and_then(Value::as_array_mut)
        .ok_or_else(invalid_response)?;
    remove_from_slice(children, rest)
}

pub(super) fn append_at(
    collection: &mut Map<String, Value>,
    parent: Option<&[usize]>,
    value: Value,
) -> Result<(), PostmanClientError> {
    if let Some(indexes) = parent {
        let items = collection
            .get_mut("item")
            .and_then(Value::as_array_mut)
            .ok_or_else(invalid_response)?;
        let folder = item_mut_at_slice(items, indexes)?;
        let children = folder
            .get_mut("item")
            .and_then(Value::as_array_mut)
            .ok_or_else(invalid_response)?;
        if children.len() >= MAX_TREE_ITEMS {
            return Err(resource_exhausted());
        }
        children.push(value);
        return Ok(());
    }
    let items = collection
        .get_mut("item")
        .and_then(Value::as_array_mut)
        .ok_or_else(invalid_response)?;
    if items.len() >= MAX_TREE_ITEMS {
        return Err(resource_exhausted());
    }
    items.push(value);
    Ok(())
}

fn item_mut_at_slice<'a>(
    items: &'a mut [Value],
    indexes: &[usize],
) -> Result<&'a mut Value, PostmanClientError> {
    let (index, rest) = indexes.split_first().ok_or_else(invalid_input)?;
    let item = items.get_mut(*index).ok_or_else(invalid_response)?;
    if rest.is_empty() {
        return Ok(item);
    }
    let children = item
        .get_mut("item")
        .and_then(Value::as_array_mut)
        .ok_or_else(invalid_response)?;
    item_mut_at_slice(children, rest)
}

pub(super) fn strip_ids(value: &mut Value) -> Result<(), PostmanClientError> {
    let root = value.as_object_mut().ok_or_else(invalid_response)?;
    let is_collection = root.contains_key("collection") || root.contains_key("info");
    let collection_or_item = if root.contains_key("collection") {
        root.get_mut("collection")
            .and_then(Value::as_object_mut)
            .ok_or_else(invalid_response)?
    } else {
        root
    };
    let mut stack = Vec::new();
    if is_collection {
        if let Some(info) = collection_or_item
            .get_mut("info")
            .and_then(Value::as_object_mut)
        {
            info.remove("_postman_id");
            info.remove("id");
            info.remove("uid");
        }
        if let Some(items) = collection_or_item
            .get_mut("item")
            .and_then(Value::as_array_mut)
        {
            stack.extend(items.iter_mut());
        }
    } else {
        collection_or_item.remove("id");
        collection_or_item.remove("uid");
        collection_or_item.remove("_postman_id");
        if let Some(items) = collection_or_item
            .get_mut("item")
            .and_then(Value::as_array_mut)
        {
            stack.extend(items.iter_mut());
        }
    }
    let mut nodes = 0usize;
    while let Some(node) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_TREE_ITEMS * MAX_TREE_DEPTH {
            return Err(resource_exhausted());
        }
        let object = node.as_object_mut().ok_or_else(invalid_response)?;
        object.remove("id");
        object.remove("uid");
        object.remove("_postman_id");
        if let Some(children) = object.get_mut("item").and_then(Value::as_array_mut) {
            stack.extend(children.iter_mut());
        }
    }
    Ok(())
}
