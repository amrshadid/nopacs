import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class UnsearchedSegments(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    UNSEARCHED_SEGMENTS_UNSPECIFIED: _ClassVar[UnsearchedSegments]
    UNSEARCHED_SEGMENTS_HOLD: _ClassVar[UnsearchedSegments]
    UNSEARCHED_SEGMENTS_KEEP: _ClassVar[UnsearchedSegments]

class UnnamedElements(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    UNNAMED_ELEMENTS_UNSPECIFIED: _ClassVar[UnnamedElements]
    UNNAMED_ELEMENTS_KEEP: _ClassVar[UnnamedElements]
    UNNAMED_ELEMENTS_REMOVE: _ClassVar[UnnamedElements]

class ConditionSubject(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CONDITION_SUBJECT_UNSPECIFIED: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_MODALITY: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_SOP_CLASS_UID: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_MANUFACTURER: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_MODEL_NAME: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_SOFTWARE_VERSIONS: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_RECEIVER: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_CALLING_AE_TITLE: _ClassVar[ConditionSubject]
    CONDITION_SUBJECT_CALLED_AE_TITLE: _ClassVar[ConditionSubject]

class LookupMissing(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    LOOKUP_MISSING_UNSPECIFIED: _ClassVar[LookupMissing]
    LOOKUP_MISSING_QUARANTINE: _ClassVar[LookupMissing]
    LOOKUP_MISSING_ASSIGN: _ClassVar[LookupMissing]

class RequiredFallback(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    REQUIRED_FALLBACK_UNSPECIFIED: _ClassVar[RequiredFallback]
    REQUIRED_FALLBACK_EMPTY: _ClassVar[RequiredFallback]
    REQUIRED_FALLBACK_DUMMY: _ClassVar[RequiredFallback]
    REQUIRED_FALLBACK_UID: _ClassVar[RequiredFallback]

class ProfileView(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROFILE_VIEW_UNSPECIFIED: _ClassVar[ProfileView]
    PROFILE_VIEW_DOCUMENT: _ClassVar[ProfileView]
    PROFILE_VIEW_FULL: _ClassVar[ProfileView]

class TrialVerdict(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TRIAL_VERDICT_UNSPECIFIED: _ClassVar[TrialVerdict]
    TRIAL_VERDICT_PASSED: _ClassVar[TrialVerdict]
    TRIAL_VERDICT_HELD: _ClassVar[TrialVerdict]
    TRIAL_VERDICT_UNDECIDED: _ClassVar[TrialVerdict]

class ProfileChange(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROFILE_CHANGE_UNSPECIFIED: _ClassVar[ProfileChange]
    PROFILE_CHANGE_ADDED: _ClassVar[ProfileChange]
    PROFILE_CHANGE_REMOVED: _ClassVar[ProfileChange]
    PROFILE_CHANGE_CHANGED: _ClassVar[ProfileChange]
    PROFILE_CHANGE_MOVED: _ClassVar[ProfileChange]

class ProfileElementClass(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PROFILE_ELEMENT_CLASS_UNSPECIFIED: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_NAMES: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_IDENTIFIERS: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_DATES: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_UIDS: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_PRIVATE: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_PIXELS: _ClassVar[ProfileElementClass]
    PROFILE_ELEMENT_CLASS_OTHER: _ClassVar[ProfileElementClass]
UNSEARCHED_SEGMENTS_UNSPECIFIED: UnsearchedSegments
UNSEARCHED_SEGMENTS_HOLD: UnsearchedSegments
UNSEARCHED_SEGMENTS_KEEP: UnsearchedSegments
UNNAMED_ELEMENTS_UNSPECIFIED: UnnamedElements
UNNAMED_ELEMENTS_KEEP: UnnamedElements
UNNAMED_ELEMENTS_REMOVE: UnnamedElements
CONDITION_SUBJECT_UNSPECIFIED: ConditionSubject
CONDITION_SUBJECT_MODALITY: ConditionSubject
CONDITION_SUBJECT_SOP_CLASS_UID: ConditionSubject
CONDITION_SUBJECT_MANUFACTURER: ConditionSubject
CONDITION_SUBJECT_MODEL_NAME: ConditionSubject
CONDITION_SUBJECT_SOFTWARE_VERSIONS: ConditionSubject
CONDITION_SUBJECT_RECEIVER: ConditionSubject
CONDITION_SUBJECT_CALLING_AE_TITLE: ConditionSubject
CONDITION_SUBJECT_CALLED_AE_TITLE: ConditionSubject
LOOKUP_MISSING_UNSPECIFIED: LookupMissing
LOOKUP_MISSING_QUARANTINE: LookupMissing
LOOKUP_MISSING_ASSIGN: LookupMissing
REQUIRED_FALLBACK_UNSPECIFIED: RequiredFallback
REQUIRED_FALLBACK_EMPTY: RequiredFallback
REQUIRED_FALLBACK_DUMMY: RequiredFallback
REQUIRED_FALLBACK_UID: RequiredFallback
PROFILE_VIEW_UNSPECIFIED: ProfileView
PROFILE_VIEW_DOCUMENT: ProfileView
PROFILE_VIEW_FULL: ProfileView
TRIAL_VERDICT_UNSPECIFIED: TrialVerdict
TRIAL_VERDICT_PASSED: TrialVerdict
TRIAL_VERDICT_HELD: TrialVerdict
TRIAL_VERDICT_UNDECIDED: TrialVerdict
PROFILE_CHANGE_UNSPECIFIED: ProfileChange
PROFILE_CHANGE_ADDED: ProfileChange
PROFILE_CHANGE_REMOVED: ProfileChange
PROFILE_CHANGE_CHANGED: ProfileChange
PROFILE_CHANGE_MOVED: ProfileChange
PROFILE_ELEMENT_CLASS_UNSPECIFIED: ProfileElementClass
PROFILE_ELEMENT_CLASS_NAMES: ProfileElementClass
PROFILE_ELEMENT_CLASS_IDENTIFIERS: ProfileElementClass
PROFILE_ELEMENT_CLASS_DATES: ProfileElementClass
PROFILE_ELEMENT_CLASS_UIDS: ProfileElementClass
PROFILE_ELEMENT_CLASS_PRIVATE: ProfileElementClass
PROFILE_ELEMENT_CLASS_PIXELS: ProfileElementClass
PROFILE_ELEMENT_CLASS_OTHER: ProfileElementClass

class ProfileDocument(_message.Message):
    __slots__ = ("bases", "rules", "unnamed", "date_shift_days", "pixel_rules", "description", "unsearched_segments", "text_lists")
    BASES_FIELD_NUMBER: _ClassVar[int]
    RULES_FIELD_NUMBER: _ClassVar[int]
    UNNAMED_FIELD_NUMBER: _ClassVar[int]
    DATE_SHIFT_DAYS_FIELD_NUMBER: _ClassVar[int]
    PIXEL_RULES_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    UNSEARCHED_SEGMENTS_FIELD_NUMBER: _ClassVar[int]
    TEXT_LISTS_FIELD_NUMBER: _ClassVar[int]
    bases: _containers.RepeatedScalarFieldContainer[str]
    rules: _containers.RepeatedCompositeFieldContainer[ProfileRule]
    unnamed: UnnamedElements
    date_shift_days: int
    pixel_rules: _containers.RepeatedCompositeFieldContainer[PixelRule]
    description: str
    unsearched_segments: UnsearchedSegments
    text_lists: _containers.RepeatedCompositeFieldContainer[TextList]
    def __init__(self, bases: _Optional[_Iterable[str]] = ..., rules: _Optional[_Iterable[_Union[ProfileRule, _Mapping]]] = ..., unnamed: _Optional[_Union[UnnamedElements, str]] = ..., date_shift_days: _Optional[int] = ..., pixel_rules: _Optional[_Iterable[_Union[PixelRule, _Mapping]]] = ..., description: _Optional[str] = ..., unsearched_segments: _Optional[_Union[UnsearchedSegments, str]] = ..., text_lists: _Optional[_Iterable[_Union[TextList, _Mapping]]] = ...) -> None: ...

class TextList(_message.Message):
    __slots__ = ("name", "version", "terms")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    TERMS_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: int
    terms: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, name: _Optional[str] = ..., version: _Optional[int] = ..., terms: _Optional[_Iterable[str]] = ...) -> None: ...

class TextListSummary(_message.Message):
    __slots__ = ("name", "version", "terms", "sha256")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    TERMS_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: int
    terms: int
    sha256: str
    def __init__(self, name: _Optional[str] = ..., version: _Optional[int] = ..., terms: _Optional[int] = ..., sha256: _Optional[str] = ...) -> None: ...

class ProfileRule(_message.Message):
    __slots__ = ("name", "selector", "when", "action", "reason")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SELECTOR_FIELD_NUMBER: _ClassVar[int]
    WHEN_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    name: str
    selector: ProfileSelector
    when: _containers.RepeatedCompositeFieldContainer[ProfileCondition]
    action: ProfileAction
    reason: str
    def __init__(self, name: _Optional[str] = ..., selector: _Optional[_Union[ProfileSelector, _Mapping]] = ..., when: _Optional[_Iterable[_Union[ProfileCondition, _Mapping]]] = ..., action: _Optional[_Union[ProfileAction, _Mapping]] = ..., reason: _Optional[str] = ...) -> None: ...

class ProfileSelector(_message.Message):
    __slots__ = ("tag", "keyword", "group", "vr", "private_element", "path", "content_item", "value_pattern", "include_private")
    TAG_FIELD_NUMBER: _ClassVar[int]
    KEYWORD_FIELD_NUMBER: _ClassVar[int]
    GROUP_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_ELEMENT_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    CONTENT_ITEM_FIELD_NUMBER: _ClassVar[int]
    VALUE_PATTERN_FIELD_NUMBER: _ClassVar[int]
    INCLUDE_PRIVATE_FIELD_NUMBER: _ClassVar[int]
    tag: str
    keyword: str
    group: str
    vr: str
    private_element: PrivateElement
    path: str
    content_item: ContentItemSelector
    value_pattern: ValuePatternSelector
    include_private: bool
    def __init__(self, tag: _Optional[str] = ..., keyword: _Optional[str] = ..., group: _Optional[str] = ..., vr: _Optional[str] = ..., private_element: _Optional[_Union[PrivateElement, _Mapping]] = ..., path: _Optional[str] = ..., content_item: _Optional[_Union[ContentItemSelector, _Mapping]] = ..., value_pattern: _Optional[_Union[ValuePatternSelector, _Mapping]] = ..., include_private: _Optional[bool] = ...) -> None: ...

class ValuePatternSelector(_message.Message):
    __slots__ = ("pattern", "search", "vr")
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    SEARCH_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    pattern: str
    search: bool
    vr: str
    def __init__(self, pattern: _Optional[str] = ..., search: _Optional[bool] = ..., vr: _Optional[str] = ...) -> None: ...

class PrivateElement(_message.Message):
    __slots__ = ("creator", "group", "element")
    CREATOR_FIELD_NUMBER: _ClassVar[int]
    GROUP_FIELD_NUMBER: _ClassVar[int]
    ELEMENT_FIELD_NUMBER: _ClassVar[int]
    creator: str
    group: str
    element: str
    def __init__(self, creator: _Optional[str] = ..., group: _Optional[str] = ..., element: _Optional[str] = ...) -> None: ...

class ContentItemSelector(_message.Message):
    __slots__ = ("scheme", "value", "value_type")
    SCHEME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    VALUE_TYPE_FIELD_NUMBER: _ClassVar[int]
    scheme: str
    value: str
    value_type: str
    def __init__(self, scheme: _Optional[str] = ..., value: _Optional[str] = ..., value_type: _Optional[str] = ...) -> None: ...

class ProfileCondition(_message.Message):
    __slots__ = ("subject", "equals", "prefix", "pattern", "present", "negate")
    SUBJECT_FIELD_NUMBER: _ClassVar[int]
    EQUALS_FIELD_NUMBER: _ClassVar[int]
    PREFIX_FIELD_NUMBER: _ClassVar[int]
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    PRESENT_FIELD_NUMBER: _ClassVar[int]
    NEGATE_FIELD_NUMBER: _ClassVar[int]
    subject: ConditionSubject
    equals: str
    prefix: str
    pattern: str
    present: bool
    negate: bool
    def __init__(self, subject: _Optional[_Union[ConditionSubject, str]] = ..., equals: _Optional[str] = ..., prefix: _Optional[str] = ..., pattern: _Optional[str] = ..., present: _Optional[bool] = ..., negate: _Optional[bool] = ...) -> None: ...

class ProfileAction(_message.Message):
    __slots__ = ("keep", "remove", "empty", "replace", "set", "hash", "uid", "shift", "generalize", "lookup", "clean", "quarantine", "dummy", "remove_unless_required", "redact")
    KEEP_FIELD_NUMBER: _ClassVar[int]
    REMOVE_FIELD_NUMBER: _ClassVar[int]
    EMPTY_FIELD_NUMBER: _ClassVar[int]
    REPLACE_FIELD_NUMBER: _ClassVar[int]
    SET_FIELD_NUMBER: _ClassVar[int]
    HASH_FIELD_NUMBER: _ClassVar[int]
    UID_FIELD_NUMBER: _ClassVar[int]
    SHIFT_FIELD_NUMBER: _ClassVar[int]
    GENERALIZE_FIELD_NUMBER: _ClassVar[int]
    LOOKUP_FIELD_NUMBER: _ClassVar[int]
    CLEAN_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_FIELD_NUMBER: _ClassVar[int]
    DUMMY_FIELD_NUMBER: _ClassVar[int]
    REMOVE_UNLESS_REQUIRED_FIELD_NUMBER: _ClassVar[int]
    REDACT_FIELD_NUMBER: _ClassVar[int]
    keep: KeepAction
    remove: RemoveAction
    empty: EmptyAction
    replace: ReplaceAction
    set: SetAction
    hash: HashAction
    uid: UidAction
    shift: ShiftAction
    generalize: GeneralizeAction
    lookup: LookupAction
    clean: CleanAction
    quarantine: QuarantineAction
    dummy: DummyAction
    remove_unless_required: RemoveUnlessRequiredAction
    redact: RedactAction
    def __init__(self, keep: _Optional[_Union[KeepAction, _Mapping]] = ..., remove: _Optional[_Union[RemoveAction, _Mapping]] = ..., empty: _Optional[_Union[EmptyAction, _Mapping]] = ..., replace: _Optional[_Union[ReplaceAction, _Mapping]] = ..., set: _Optional[_Union[SetAction, _Mapping]] = ..., hash: _Optional[_Union[HashAction, _Mapping]] = ..., uid: _Optional[_Union[UidAction, _Mapping]] = ..., shift: _Optional[_Union[ShiftAction, _Mapping]] = ..., generalize: _Optional[_Union[GeneralizeAction, _Mapping]] = ..., lookup: _Optional[_Union[LookupAction, _Mapping]] = ..., clean: _Optional[_Union[CleanAction, _Mapping]] = ..., quarantine: _Optional[_Union[QuarantineAction, _Mapping]] = ..., dummy: _Optional[_Union[DummyAction, _Mapping]] = ..., remove_unless_required: _Optional[_Union[RemoveUnlessRequiredAction, _Mapping]] = ..., redact: _Optional[_Union[RedactAction, _Mapping]] = ...) -> None: ...

class KeepAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class RemoveAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class EmptyAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ReplaceAction(_message.Message):
    __slots__ = ("constant", "template")
    CONSTANT_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    constant: str
    template: str
    def __init__(self, constant: _Optional[str] = ..., template: _Optional[str] = ...) -> None: ...

class SetAction(_message.Message):
    __slots__ = ("constant", "template")
    CONSTANT_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    constant: str
    template: str
    def __init__(self, constant: _Optional[str] = ..., template: _Optional[str] = ...) -> None: ...

class HashAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class UidAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ShiftAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GeneralizeAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class LookupAction(_message.Message):
    __slots__ = ("table", "on_missing")
    TABLE_FIELD_NUMBER: _ClassVar[int]
    ON_MISSING_FIELD_NUMBER: _ClassVar[int]
    table: str
    on_missing: LookupMissing
    def __init__(self, table: _Optional[str] = ..., on_missing: _Optional[_Union[LookupMissing, str]] = ...) -> None: ...

class CleanAction(_message.Message):
    __slots__ = ("lists",)
    LISTS_FIELD_NUMBER: _ClassVar[int]
    lists: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, lists: _Optional[_Iterable[str]] = ...) -> None: ...

class QuarantineAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DummyAction(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class RedactAction(_message.Message):
    __slots__ = ("pattern", "replacement", "lists")
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    REPLACEMENT_FIELD_NUMBER: _ClassVar[int]
    LISTS_FIELD_NUMBER: _ClassVar[int]
    pattern: str
    replacement: str
    lists: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, pattern: _Optional[str] = ..., replacement: _Optional[str] = ..., lists: _Optional[_Iterable[str]] = ...) -> None: ...

class RemoveUnlessRequiredAction(_message.Message):
    __slots__ = ("fallback",)
    FALLBACK_FIELD_NUMBER: _ClassVar[int]
    fallback: RequiredFallback
    def __init__(self, fallback: _Optional[_Union[RequiredFallback, str]] = ...) -> None: ...

class PixelRule(_message.Message):
    __slots__ = ("name", "when", "rows", "columns", "redact", "detect", "accept", "redact_by_template", "reason")
    NAME_FIELD_NUMBER: _ClassVar[int]
    WHEN_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    COLUMNS_FIELD_NUMBER: _ClassVar[int]
    REDACT_FIELD_NUMBER: _ClassVar[int]
    DETECT_FIELD_NUMBER: _ClassVar[int]
    ACCEPT_FIELD_NUMBER: _ClassVar[int]
    REDACT_BY_TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    name: str
    when: _containers.RepeatedCompositeFieldContainer[ProfileCondition]
    rows: int
    columns: int
    redact: RedactPixels
    detect: DetectText
    accept: AcceptPixels
    redact_by_template: RedactByTemplate
    reason: str
    def __init__(self, name: _Optional[str] = ..., when: _Optional[_Iterable[_Union[ProfileCondition, _Mapping]]] = ..., rows: _Optional[int] = ..., columns: _Optional[int] = ..., redact: _Optional[_Union[RedactPixels, _Mapping]] = ..., detect: _Optional[_Union[DetectText, _Mapping]] = ..., accept: _Optional[_Union[AcceptPixels, _Mapping]] = ..., redact_by_template: _Optional[_Union[RedactByTemplate, _Mapping]] = ..., reason: _Optional[str] = ...) -> None: ...

class RedactPixels(_message.Message):
    __slots__ = ("rectangles",)
    RECTANGLES_FIELD_NUMBER: _ClassVar[int]
    rectangles: _containers.RepeatedCompositeFieldContainer[PixelRectangle]
    def __init__(self, rectangles: _Optional[_Iterable[_Union[PixelRectangle, _Mapping]]] = ...) -> None: ...

class PixelRectangle(_message.Message):
    __slots__ = ("x", "y", "width", "height", "from_right", "from_bottom")
    X_FIELD_NUMBER: _ClassVar[int]
    Y_FIELD_NUMBER: _ClassVar[int]
    WIDTH_FIELD_NUMBER: _ClassVar[int]
    HEIGHT_FIELD_NUMBER: _ClassVar[int]
    FROM_RIGHT_FIELD_NUMBER: _ClassVar[int]
    FROM_BOTTOM_FIELD_NUMBER: _ClassVar[int]
    x: int
    y: int
    width: int
    height: int
    from_right: bool
    from_bottom: bool
    def __init__(self, x: _Optional[int] = ..., y: _Optional[int] = ..., width: _Optional[int] = ..., height: _Optional[int] = ..., from_right: _Optional[bool] = ..., from_bottom: _Optional[bool] = ...) -> None: ...

class RedactByTemplate(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DetectText(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class AcceptPixels(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ExpandedProfile(_message.Message):
    __slots__ = ("rules", "unnamed", "date_shift_days", "pixel_rules", "bases", "language_edition", "unsearched_segments", "text_lists", "layers")
    RULES_FIELD_NUMBER: _ClassVar[int]
    UNNAMED_FIELD_NUMBER: _ClassVar[int]
    DATE_SHIFT_DAYS_FIELD_NUMBER: _ClassVar[int]
    PIXEL_RULES_FIELD_NUMBER: _ClassVar[int]
    BASES_FIELD_NUMBER: _ClassVar[int]
    LANGUAGE_EDITION_FIELD_NUMBER: _ClassVar[int]
    UNSEARCHED_SEGMENTS_FIELD_NUMBER: _ClassVar[int]
    TEXT_LISTS_FIELD_NUMBER: _ClassVar[int]
    LAYERS_FIELD_NUMBER: _ClassVar[int]
    rules: _containers.RepeatedCompositeFieldContainer[ExpandedRule]
    unnamed: UnnamedElements
    date_shift_days: int
    pixel_rules: _containers.RepeatedCompositeFieldContainer[ExpandedPixelRule]
    bases: _containers.RepeatedScalarFieldContainer[str]
    language_edition: int
    unsearched_segments: UnsearchedSegments
    text_lists: _containers.RepeatedCompositeFieldContainer[TextList]
    layers: _containers.RepeatedCompositeFieldContainer[ExpandedLayer]
    def __init__(self, rules: _Optional[_Iterable[_Union[ExpandedRule, _Mapping]]] = ..., unnamed: _Optional[_Union[UnnamedElements, str]] = ..., date_shift_days: _Optional[int] = ..., pixel_rules: _Optional[_Iterable[_Union[ExpandedPixelRule, _Mapping]]] = ..., bases: _Optional[_Iterable[str]] = ..., language_edition: _Optional[int] = ..., unsearched_segments: _Optional[_Union[UnsearchedSegments, str]] = ..., text_lists: _Optional[_Iterable[_Union[TextList, _Mapping]]] = ..., layers: _Optional[_Iterable[_Union[ExpandedLayer, _Mapping]]] = ...) -> None: ...

class ExpandedLayer(_message.Message):
    __slots__ = ("rules", "pixel_rules", "unnamed")
    RULES_FIELD_NUMBER: _ClassVar[int]
    PIXEL_RULES_FIELD_NUMBER: _ClassVar[int]
    UNNAMED_FIELD_NUMBER: _ClassVar[int]
    rules: int
    pixel_rules: int
    unnamed: UnnamedElements
    def __init__(self, rules: _Optional[int] = ..., pixel_rules: _Optional[int] = ..., unnamed: _Optional[_Union[UnnamedElements, str]] = ...) -> None: ...

class ExpandedRule(_message.Message):
    __slots__ = ("rule", "origin", "vrs")
    RULE_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    VRS_FIELD_NUMBER: _ClassVar[int]
    rule: ProfileRule
    origin: str
    vrs: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, rule: _Optional[_Union[ProfileRule, _Mapping]] = ..., origin: _Optional[str] = ..., vrs: _Optional[_Iterable[str]] = ...) -> None: ...

class ExpandedPixelRule(_message.Message):
    __slots__ = ("rule", "origin")
    RULE_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    rule: PixelRule
    origin: str
    def __init__(self, rule: _Optional[_Union[PixelRule, _Mapping]] = ..., origin: _Optional[str] = ...) -> None: ...

class ProfileVersion(_message.Message):
    __slots__ = ("name", "version", "checksum", "create_time", "created_by_key_id", "note", "document", "expanded", "terms_withheld")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    CREATED_BY_KEY_ID_FIELD_NUMBER: _ClassVar[int]
    NOTE_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    EXPANDED_FIELD_NUMBER: _ClassVar[int]
    TERMS_WITHHELD_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: int
    checksum: str
    create_time: _timestamp_pb2.Timestamp
    created_by_key_id: str
    note: str
    document: ProfileDocument
    expanded: ExpandedProfile
    terms_withheld: bool
    def __init__(self, name: _Optional[str] = ..., version: _Optional[int] = ..., checksum: _Optional[str] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., created_by_key_id: _Optional[str] = ..., note: _Optional[str] = ..., document: _Optional[_Union[ProfileDocument, _Mapping]] = ..., expanded: _Optional[_Union[ExpandedProfile, _Mapping]] = ..., terms_withheld: _Optional[bool] = ...) -> None: ...

class ProfileSummary(_message.Message):
    __slots__ = ("name", "latest_version", "latest_create_time")
    NAME_FIELD_NUMBER: _ClassVar[int]
    LATEST_VERSION_FIELD_NUMBER: _ClassVar[int]
    LATEST_CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    name: str
    latest_version: int
    latest_create_time: _timestamp_pb2.Timestamp
    def __init__(self, name: _Optional[str] = ..., latest_version: _Optional[int] = ..., latest_create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ProfileProblem(_message.Message):
    __slots__ = ("rule", "pixel_rule", "rule_name", "field", "code", "message")
    RULE_FIELD_NUMBER: _ClassVar[int]
    PIXEL_RULE_FIELD_NUMBER: _ClassVar[int]
    RULE_NAME_FIELD_NUMBER: _ClassVar[int]
    FIELD_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    rule: int
    pixel_rule: int
    rule_name: str
    field: str
    code: str
    message: str
    def __init__(self, rule: _Optional[int] = ..., pixel_rule: _Optional[int] = ..., rule_name: _Optional[str] = ..., field: _Optional[str] = ..., code: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...

class ProfileProblems(_message.Message):
    __slots__ = ("problems",)
    PROBLEMS_FIELD_NUMBER: _ClassVar[int]
    problems: _containers.RepeatedCompositeFieldContainer[ProfileProblem]
    def __init__(self, problems: _Optional[_Iterable[_Union[ProfileProblem, _Mapping]]] = ...) -> None: ...

class ProfileBase(_message.Message):
    __slots__ = ("name", "available", "unavailable_reason", "description", "document")
    NAME_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_FIELD_NUMBER: _ClassVar[int]
    UNAVAILABLE_REASON_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    available: bool
    unavailable_reason: str
    description: str
    document: ProfileDocument
    def __init__(self, name: _Optional[str] = ..., available: _Optional[bool] = ..., unavailable_reason: _Optional[str] = ..., description: _Optional[str] = ..., document: _Optional[_Union[ProfileDocument, _Mapping]] = ...) -> None: ...

class CreateProfileVersionRequest(_message.Message):
    __slots__ = ("tenant", "name", "document", "note", "expected_latest_version", "view")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    NOTE_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_LATEST_VERSION_FIELD_NUMBER: _ClassVar[int]
    VIEW_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    document: ProfileDocument
    note: str
    expected_latest_version: int
    view: ProfileView
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ..., document: _Optional[_Union[ProfileDocument, _Mapping]] = ..., note: _Optional[str] = ..., expected_latest_version: _Optional[int] = ..., view: _Optional[_Union[ProfileView, str]] = ...) -> None: ...

class CreateProfileVersionResponse(_message.Message):
    __slots__ = ("version", "created", "warnings", "note_differs")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CREATED_FIELD_NUMBER: _ClassVar[int]
    WARNINGS_FIELD_NUMBER: _ClassVar[int]
    NOTE_DIFFERS_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersion
    created: bool
    warnings: _containers.RepeatedCompositeFieldContainer[ProfileProblem]
    note_differs: bool
    def __init__(self, version: _Optional[_Union[ProfileVersion, _Mapping]] = ..., created: _Optional[bool] = ..., warnings: _Optional[_Iterable[_Union[ProfileProblem, _Mapping]]] = ..., note_differs: _Optional[bool] = ...) -> None: ...

class GetProfileVersionRequest(_message.Message):
    __slots__ = ("tenant", "name", "version", "view")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    VIEW_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    version: int
    view: ProfileView
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ..., version: _Optional[int] = ..., view: _Optional[_Union[ProfileView, str]] = ...) -> None: ...

class GetProfileVersionResponse(_message.Message):
    __slots__ = ("version",)
    VERSION_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersion
    def __init__(self, version: _Optional[_Union[ProfileVersion, _Mapping]] = ...) -> None: ...

class ListProfilesRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListProfilesResponse(_message.Message):
    __slots__ = ("profiles", "next_page_token")
    PROFILES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    profiles: _containers.RepeatedCompositeFieldContainer[ProfileSummary]
    next_page_token: str
    def __init__(self, profiles: _Optional[_Iterable[_Union[ProfileSummary, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ListProfileVersionsRequest(_message.Message):
    __slots__ = ("tenant", "name", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListProfileVersionsResponse(_message.Message):
    __slots__ = ("versions", "next_page_token")
    VERSIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    versions: _containers.RepeatedCompositeFieldContainer[ProfileVersion]
    next_page_token: str
    def __init__(self, versions: _Optional[_Iterable[_Union[ProfileVersion, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ValidateProfileRequest(_message.Message):
    __slots__ = ("tenant", "document", "view")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    VIEW_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    document: ProfileDocument
    view: ProfileView
    def __init__(self, tenant: _Optional[str] = ..., document: _Optional[_Union[ProfileDocument, _Mapping]] = ..., view: _Optional[_Union[ProfileView, str]] = ...) -> None: ...

class ValidateProfileResponse(_message.Message):
    __slots__ = ("problems", "expanded", "checksum", "warnings")
    PROBLEMS_FIELD_NUMBER: _ClassVar[int]
    EXPANDED_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    WARNINGS_FIELD_NUMBER: _ClassVar[int]
    problems: _containers.RepeatedCompositeFieldContainer[ProfileProblem]
    expanded: ExpandedProfile
    checksum: str
    warnings: _containers.RepeatedCompositeFieldContainer[ProfileProblem]
    def __init__(self, problems: _Optional[_Iterable[_Union[ProfileProblem, _Mapping]]] = ..., expanded: _Optional[_Union[ExpandedProfile, _Mapping]] = ..., checksum: _Optional[str] = ..., warnings: _Optional[_Iterable[_Union[ProfileProblem, _Mapping]]] = ...) -> None: ...

class ListProfileBasesRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListProfileBasesResponse(_message.Message):
    __slots__ = ("bases",)
    BASES_FIELD_NUMBER: _ClassVar[int]
    bases: _containers.RepeatedCompositeFieldContainer[ProfileBase]
    def __init__(self, bases: _Optional[_Iterable[_Union[ProfileBase, _Mapping]]] = ...) -> None: ...

class TrialProfile(_message.Message):
    __slots__ = ("version", "document")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersionName
    document: ProfileDocument
    def __init__(self, version: _Optional[_Union[ProfileVersionName, _Mapping]] = ..., document: _Optional[_Union[ProfileDocument, _Mapping]] = ...) -> None: ...

class ProfileVersionName(_message.Message):
    __slots__ = ("name", "version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: int
    def __init__(self, name: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class TrialFile(_message.Message):
    __slots__ = ("name", "content")
    NAME_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    content: bytes
    def __init__(self, name: _Optional[str] = ..., content: _Optional[bytes] = ...) -> None: ...

class TrialReception(_message.Message):
    __slots__ = ("receiver", "calling_ae_title", "called_ae_title")
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    CALLING_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    CALLED_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    receiver: str
    calling_ae_title: str
    called_ae_title: str
    def __init__(self, receiver: _Optional[str] = ..., calling_ae_title: _Optional[str] = ..., called_ae_title: _Optional[str] = ...) -> None: ...

class TryProfileRequest(_message.Message):
    __slots__ = ("tenant", "profile", "files", "quarantine_records", "reception", "show_values")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_RECORDS_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    SHOW_VALUES_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    profile: TrialProfile
    files: _containers.RepeatedCompositeFieldContainer[TrialFile]
    quarantine_records: _containers.RepeatedScalarFieldContainer[str]
    reception: TrialReception
    show_values: bool
    def __init__(self, tenant: _Optional[str] = ..., profile: _Optional[_Union[TrialProfile, _Mapping]] = ..., files: _Optional[_Iterable[_Union[TrialFile, _Mapping]]] = ..., quarantine_records: _Optional[_Iterable[str]] = ..., reception: _Optional[_Union[TrialReception, _Mapping]] = ..., show_values: _Optional[bool] = ...) -> None: ...

class TryProfileResponse(_message.Message):
    __slots__ = ("version", "checksum", "reception_refusal", "results", "summary")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_REFUSAL_FIELD_NUMBER: _ClassVar[int]
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersion
    checksum: str
    reception_refusal: str
    results: _containers.RepeatedCompositeFieldContainer[TrialResult]
    summary: TrialSummary
    def __init__(self, version: _Optional[_Union[ProfileVersion, _Mapping]] = ..., checksum: _Optional[str] = ..., reception_refusal: _Optional[str] = ..., results: _Optional[_Iterable[_Union[TrialResult, _Mapping]]] = ..., summary: _Optional[_Union[TrialSummary, _Mapping]] = ...) -> None: ...

class TrialResult(_message.Message):
    __slots__ = ("name", "quarantine_record", "verdict", "reason", "cause", "path", "rule", "elements", "elements_omitted", "values_searched", "values_skipped", "lookups_unassigned", "receiver", "deidentification_only", "pixels", "route", "routed_profile")
    NAME_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_RECORD_FIELD_NUMBER: _ClassVar[int]
    VERDICT_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    CAUSE_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_OMITTED_FIELD_NUMBER: _ClassVar[int]
    VALUES_SEARCHED_FIELD_NUMBER: _ClassVar[int]
    VALUES_SKIPPED_FIELD_NUMBER: _ClassVar[int]
    LOOKUPS_UNASSIGNED_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    DEIDENTIFICATION_ONLY_FIELD_NUMBER: _ClassVar[int]
    PIXELS_FIELD_NUMBER: _ClassVar[int]
    ROUTE_FIELD_NUMBER: _ClassVar[int]
    ROUTED_PROFILE_FIELD_NUMBER: _ClassVar[int]
    name: str
    quarantine_record: str
    verdict: TrialVerdict
    reason: str
    cause: str
    path: str
    rule: str
    elements: _containers.RepeatedCompositeFieldContainer[TrialElement]
    elements_omitted: int
    values_searched: int
    values_skipped: int
    lookups_unassigned: int
    receiver: str
    deidentification_only: bool
    pixels: TrialPixels
    route: str
    routed_profile: ProfileVersionName
    def __init__(self, name: _Optional[str] = ..., quarantine_record: _Optional[str] = ..., verdict: _Optional[_Union[TrialVerdict, str]] = ..., reason: _Optional[str] = ..., cause: _Optional[str] = ..., path: _Optional[str] = ..., rule: _Optional[str] = ..., elements: _Optional[_Iterable[_Union[TrialElement, _Mapping]]] = ..., elements_omitted: _Optional[int] = ..., values_searched: _Optional[int] = ..., values_skipped: _Optional[int] = ..., lookups_unassigned: _Optional[int] = ..., receiver: _Optional[str] = ..., deidentification_only: _Optional[bool] = ..., pixels: _Optional[_Union[TrialPixels, _Mapping]] = ..., route: _Optional[str] = ..., routed_profile: _Optional[_Union[ProfileVersionName, _Mapping]] = ...) -> None: ...

class TrialPixels(_message.Message):
    __slots__ = ("at_risk", "risk_cause", "rule", "origin", "action", "regions", "frames", "decoded_bytes", "decoded", "segments_removed")
    AT_RISK_FIELD_NUMBER: _ClassVar[int]
    RISK_CAUSE_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    REGIONS_FIELD_NUMBER: _ClassVar[int]
    FRAMES_FIELD_NUMBER: _ClassVar[int]
    DECODED_BYTES_FIELD_NUMBER: _ClassVar[int]
    DECODED_FIELD_NUMBER: _ClassVar[int]
    SEGMENTS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    at_risk: bool
    risk_cause: str
    rule: str
    origin: str
    action: str
    regions: _containers.RepeatedCompositeFieldContainer[PixelRectangle]
    frames: int
    decoded_bytes: int
    decoded: bool
    segments_removed: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, at_risk: _Optional[bool] = ..., risk_cause: _Optional[str] = ..., rule: _Optional[str] = ..., origin: _Optional[str] = ..., action: _Optional[str] = ..., regions: _Optional[_Iterable[_Union[PixelRectangle, _Mapping]]] = ..., frames: _Optional[int] = ..., decoded_bytes: _Optional[int] = ..., decoded: _Optional[bool] = ..., segments_removed: _Optional[_Iterable[str]] = ...) -> None: ...

class TrialElement(_message.Message):
    __slots__ = ("path", "keyword", "vr", "action", "rule", "rule_name", "origin", "reason", "changed", "before", "after", "present_after", "redactions", "pattern_matches", "before_length", "after_length")
    PATH_FIELD_NUMBER: _ClassVar[int]
    KEYWORD_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    RULE_NAME_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    CHANGED_FIELD_NUMBER: _ClassVar[int]
    BEFORE_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    PRESENT_AFTER_FIELD_NUMBER: _ClassVar[int]
    REDACTIONS_FIELD_NUMBER: _ClassVar[int]
    PATTERN_MATCHES_FIELD_NUMBER: _ClassVar[int]
    BEFORE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    AFTER_LENGTH_FIELD_NUMBER: _ClassVar[int]
    path: str
    keyword: str
    vr: str
    action: str
    rule: int
    rule_name: str
    origin: str
    reason: str
    changed: bool
    before: str
    after: str
    present_after: bool
    redactions: int
    pattern_matches: int
    before_length: int
    after_length: int
    def __init__(self, path: _Optional[str] = ..., keyword: _Optional[str] = ..., vr: _Optional[str] = ..., action: _Optional[str] = ..., rule: _Optional[int] = ..., rule_name: _Optional[str] = ..., origin: _Optional[str] = ..., reason: _Optional[str] = ..., changed: _Optional[bool] = ..., before: _Optional[str] = ..., after: _Optional[str] = ..., present_after: _Optional[bool] = ..., redactions: _Optional[int] = ..., pattern_matches: _Optional[int] = ..., before_length: _Optional[int] = ..., after_length: _Optional[int] = ...) -> None: ...

class TrialSummary(_message.Message):
    __slots__ = ("files", "passed", "held", "by_rule", "by_reason", "undecided")
    FILES_FIELD_NUMBER: _ClassVar[int]
    PASSED_FIELD_NUMBER: _ClassVar[int]
    HELD_FIELD_NUMBER: _ClassVar[int]
    BY_RULE_FIELD_NUMBER: _ClassVar[int]
    BY_REASON_FIELD_NUMBER: _ClassVar[int]
    UNDECIDED_FIELD_NUMBER: _ClassVar[int]
    files: int
    passed: int
    held: int
    by_rule: _containers.RepeatedCompositeFieldContainer[TrialCount]
    by_reason: _containers.RepeatedCompositeFieldContainer[TrialCount]
    undecided: int
    def __init__(self, files: _Optional[int] = ..., passed: _Optional[int] = ..., held: _Optional[int] = ..., by_rule: _Optional[_Iterable[_Union[TrialCount, _Mapping]]] = ..., by_reason: _Optional[_Iterable[_Union[TrialCount, _Mapping]]] = ..., undecided: _Optional[int] = ...) -> None: ...

class TrialCount(_message.Message):
    __slots__ = ("key", "count")
    KEY_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    key: str
    count: int
    def __init__(self, key: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...

class ExplainProfileRequest(_message.Message):
    __slots__ = ("tenant", "profile", "file", "quarantine_record", "reception")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    FILE_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_RECORD_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    profile: TrialProfile
    file: TrialFile
    quarantine_record: str
    reception: TrialReception
    def __init__(self, tenant: _Optional[str] = ..., profile: _Optional[_Union[TrialProfile, _Mapping]] = ..., file: _Optional[_Union[TrialFile, _Mapping]] = ..., quarantine_record: _Optional[str] = ..., reception: _Optional[_Union[TrialReception, _Mapping]] = ...) -> None: ...

class ExplainProfileResponse(_message.Message):
    __slots__ = ("version", "checksum", "elements", "elements_omitted", "refusal", "refusal_path")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_OMITTED_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_PATH_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersion
    checksum: str
    elements: _containers.RepeatedCompositeFieldContainer[ExplainedElement]
    elements_omitted: int
    refusal: str
    refusal_path: str
    def __init__(self, version: _Optional[_Union[ProfileVersion, _Mapping]] = ..., checksum: _Optional[str] = ..., elements: _Optional[_Iterable[_Union[ExplainedElement, _Mapping]]] = ..., elements_omitted: _Optional[int] = ..., refusal: _Optional[str] = ..., refusal_path: _Optional[str] = ...) -> None: ...

class ExplainedElement(_message.Message):
    __slots__ = ("path", "keyword", "vr", "action", "rule", "rule_name", "origin", "reason", "selected_by", "others", "others_omitted")
    PATH_FIELD_NUMBER: _ClassVar[int]
    KEYWORD_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    RULE_NAME_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    SELECTED_BY_FIELD_NUMBER: _ClassVar[int]
    OTHERS_FIELD_NUMBER: _ClassVar[int]
    OTHERS_OMITTED_FIELD_NUMBER: _ClassVar[int]
    path: str
    keyword: str
    vr: str
    action: str
    rule: int
    rule_name: str
    origin: str
    reason: str
    selected_by: str
    others: _containers.RepeatedCompositeFieldContainer[ExplainedCandidate]
    others_omitted: int
    def __init__(self, path: _Optional[str] = ..., keyword: _Optional[str] = ..., vr: _Optional[str] = ..., action: _Optional[str] = ..., rule: _Optional[int] = ..., rule_name: _Optional[str] = ..., origin: _Optional[str] = ..., reason: _Optional[str] = ..., selected_by: _Optional[str] = ..., others: _Optional[_Iterable[_Union[ExplainedCandidate, _Mapping]]] = ..., others_omitted: _Optional[int] = ...) -> None: ...

class ExplainedCandidate(_message.Message):
    __slots__ = ("rule", "rule_name", "origin", "selected_by", "why_not")
    RULE_FIELD_NUMBER: _ClassVar[int]
    RULE_NAME_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    SELECTED_BY_FIELD_NUMBER: _ClassVar[int]
    WHY_NOT_FIELD_NUMBER: _ClassVar[int]
    rule: int
    rule_name: str
    origin: str
    selected_by: str
    why_not: str
    def __init__(self, rule: _Optional[int] = ..., rule_name: _Optional[str] = ..., origin: _Optional[str] = ..., selected_by: _Optional[str] = ..., why_not: _Optional[str] = ...) -> None: ...

class DiffProfileVersionsRequest(_message.Message):
    __slots__ = ("tenant", "name", "from_version", "to_version", "to_name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    FROM_VERSION_FIELD_NUMBER: _ClassVar[int]
    TO_VERSION_FIELD_NUMBER: _ClassVar[int]
    TO_NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    from_version: int
    to_version: int
    to_name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ..., from_version: _Optional[int] = ..., to_version: _Optional[int] = ..., to_name: _Optional[str] = ...) -> None: ...

class DiffProfileVersionsResponse(_message.Message):
    __slots__ = ("to", "same_effect", "bases_added", "bases_removed", "unnamed_from", "unnamed_to", "date_shift_days_from", "date_shift_days_to", "rules", "rules_omitted", "effects", "effects_omitted", "pixel_rules_changed", "bases_reordered", "unlisted_change", "unsearched_segments_from", "unsearched_segments_to", "text_lists")
    FROM_FIELD_NUMBER: _ClassVar[int]
    TO_FIELD_NUMBER: _ClassVar[int]
    SAME_EFFECT_FIELD_NUMBER: _ClassVar[int]
    BASES_ADDED_FIELD_NUMBER: _ClassVar[int]
    BASES_REMOVED_FIELD_NUMBER: _ClassVar[int]
    UNNAMED_FROM_FIELD_NUMBER: _ClassVar[int]
    UNNAMED_TO_FIELD_NUMBER: _ClassVar[int]
    DATE_SHIFT_DAYS_FROM_FIELD_NUMBER: _ClassVar[int]
    DATE_SHIFT_DAYS_TO_FIELD_NUMBER: _ClassVar[int]
    RULES_FIELD_NUMBER: _ClassVar[int]
    RULES_OMITTED_FIELD_NUMBER: _ClassVar[int]
    EFFECTS_FIELD_NUMBER: _ClassVar[int]
    EFFECTS_OMITTED_FIELD_NUMBER: _ClassVar[int]
    PIXEL_RULES_CHANGED_FIELD_NUMBER: _ClassVar[int]
    BASES_REORDERED_FIELD_NUMBER: _ClassVar[int]
    UNLISTED_CHANGE_FIELD_NUMBER: _ClassVar[int]
    UNSEARCHED_SEGMENTS_FROM_FIELD_NUMBER: _ClassVar[int]
    UNSEARCHED_SEGMENTS_TO_FIELD_NUMBER: _ClassVar[int]
    TEXT_LISTS_FIELD_NUMBER: _ClassVar[int]
    to: ProfileVersion
    same_effect: bool
    bases_added: _containers.RepeatedScalarFieldContainer[str]
    bases_removed: _containers.RepeatedScalarFieldContainer[str]
    unnamed_from: UnnamedElements
    unnamed_to: UnnamedElements
    date_shift_days_from: int
    date_shift_days_to: int
    rules: _containers.RepeatedCompositeFieldContainer[ProfileRuleChange]
    rules_omitted: int
    effects: _containers.RepeatedCompositeFieldContainer[ProfileEffectChange]
    effects_omitted: int
    pixel_rules_changed: int
    bases_reordered: bool
    unlisted_change: bool
    unsearched_segments_from: UnsearchedSegments
    unsearched_segments_to: UnsearchedSegments
    text_lists: _containers.RepeatedCompositeFieldContainer[TextListChange]
    def __init__(self, to: _Optional[_Union[ProfileVersion, _Mapping]] = ..., same_effect: _Optional[bool] = ..., bases_added: _Optional[_Iterable[str]] = ..., bases_removed: _Optional[_Iterable[str]] = ..., unnamed_from: _Optional[_Union[UnnamedElements, str]] = ..., unnamed_to: _Optional[_Union[UnnamedElements, str]] = ..., date_shift_days_from: _Optional[int] = ..., date_shift_days_to: _Optional[int] = ..., rules: _Optional[_Iterable[_Union[ProfileRuleChange, _Mapping]]] = ..., rules_omitted: _Optional[int] = ..., effects: _Optional[_Iterable[_Union[ProfileEffectChange, _Mapping]]] = ..., effects_omitted: _Optional[int] = ..., pixel_rules_changed: _Optional[int] = ..., bases_reordered: _Optional[bool] = ..., unlisted_change: _Optional[bool] = ..., unsearched_segments_from: _Optional[_Union[UnsearchedSegments, str]] = ..., unsearched_segments_to: _Optional[_Union[UnsearchedSegments, str]] = ..., text_lists: _Optional[_Iterable[_Union[TextListChange, _Mapping]]] = ..., **kwargs) -> None: ...

class TextListChange(_message.Message):
    __slots__ = ("name", "change", "to")
    NAME_FIELD_NUMBER: _ClassVar[int]
    CHANGE_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    TO_FIELD_NUMBER: _ClassVar[int]
    name: str
    change: ProfileChange
    to: TextListSummary
    def __init__(self, name: _Optional[str] = ..., change: _Optional[_Union[ProfileChange, str]] = ..., to: _Optional[_Union[TextListSummary, _Mapping]] = ..., **kwargs) -> None: ...

class ProfileRuleChange(_message.Message):
    __slots__ = ("change", "origin", "from_position", "to_position", "to")
    CHANGE_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    FROM_POSITION_FIELD_NUMBER: _ClassVar[int]
    TO_POSITION_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    TO_FIELD_NUMBER: _ClassVar[int]
    change: ProfileChange
    origin: str
    from_position: int
    to_position: int
    to: ProfileRule
    def __init__(self, change: _Optional[_Union[ProfileChange, str]] = ..., origin: _Optional[str] = ..., from_position: _Optional[int] = ..., to_position: _Optional[int] = ..., to: _Optional[_Union[ProfileRule, _Mapping]] = ..., **kwargs) -> None: ...

class ProfileEffectChange(_message.Message):
    __slots__ = ("selector", "when", "to", "from_position", "to_position", "order_changed")
    SELECTOR_FIELD_NUMBER: _ClassVar[int]
    WHEN_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    TO_FIELD_NUMBER: _ClassVar[int]
    FROM_POSITION_FIELD_NUMBER: _ClassVar[int]
    TO_POSITION_FIELD_NUMBER: _ClassVar[int]
    ORDER_CHANGED_FIELD_NUMBER: _ClassVar[int]
    selector: ProfileSelector
    when: _containers.RepeatedCompositeFieldContainer[ProfileCondition]
    to: ProfileAction
    from_position: int
    to_position: int
    order_changed: bool
    def __init__(self, selector: _Optional[_Union[ProfileSelector, _Mapping]] = ..., when: _Optional[_Iterable[_Union[ProfileCondition, _Mapping]]] = ..., to: _Optional[_Union[ProfileAction, _Mapping]] = ..., from_position: _Optional[int] = ..., to_position: _Optional[int] = ..., order_changed: _Optional[bool] = ..., **kwargs) -> None: ...

class DescribeProfileRequest(_message.Message):
    __slots__ = ("tenant", "profile", "effective_receiver")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_RECEIVER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    profile: TrialProfile
    effective_receiver: str
    def __init__(self, tenant: _Optional[str] = ..., profile: _Optional[_Union[TrialProfile, _Mapping]] = ..., effective_receiver: _Optional[str] = ...) -> None: ...

class DescribeProfileResponse(_message.Message):
    __slots__ = ("version", "checksum", "classes", "notes", "method", "text_lists", "effective")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    CLASSES_FIELD_NUMBER: _ClassVar[int]
    NOTES_FIELD_NUMBER: _ClassVar[int]
    METHOD_FIELD_NUMBER: _ClassVar[int]
    TEXT_LISTS_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    version: ProfileVersion
    checksum: str
    classes: _containers.RepeatedCompositeFieldContainer[ProfileClassEffects]
    notes: _containers.RepeatedScalarFieldContainer[str]
    method: ProfileMethod
    text_lists: _containers.RepeatedCompositeFieldContainer[TextListSummary]
    effective: _containers.RepeatedCompositeFieldContainer[EffectiveElement]
    def __init__(self, version: _Optional[_Union[ProfileVersion, _Mapping]] = ..., checksum: _Optional[str] = ..., classes: _Optional[_Iterable[_Union[ProfileClassEffects, _Mapping]]] = ..., notes: _Optional[_Iterable[str]] = ..., method: _Optional[_Union[ProfileMethod, _Mapping]] = ..., text_lists: _Optional[_Iterable[_Union[TextListSummary, _Mapping]]] = ..., effective: _Optional[_Iterable[_Union[EffectiveElement, _Mapping]]] = ...) -> None: ...

class EffectiveElement(_message.Message):
    __slots__ = ("what", "action", "conditional", "layer", "layer_profile", "rule", "rule_name", "origin")
    WHAT_FIELD_NUMBER: _ClassVar[int]
    CLASS_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    CONDITIONAL_FIELD_NUMBER: _ClassVar[int]
    LAYER_FIELD_NUMBER: _ClassVar[int]
    LAYER_PROFILE_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    RULE_NAME_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    what: str
    action: str
    conditional: bool
    layer: str
    layer_profile: str
    rule: int
    rule_name: str
    origin: str
    def __init__(self, what: _Optional[str] = ..., action: _Optional[str] = ..., conditional: _Optional[bool] = ..., layer: _Optional[str] = ..., layer_profile: _Optional[str] = ..., rule: _Optional[int] = ..., rule_name: _Optional[str] = ..., origin: _Optional[str] = ..., **kwargs) -> None: ...

class ProfileClassEffects(_message.Message):
    __slots__ = ("effects",)
    CLASS_FIELD_NUMBER: _ClassVar[int]
    EFFECTS_FIELD_NUMBER: _ClassVar[int]
    effects: _containers.RepeatedCompositeFieldContainer[ProfileClassEffect]
    def __init__(self, effects: _Optional[_Iterable[_Union[ProfileClassEffect, _Mapping]]] = ..., **kwargs) -> None: ...

class ProfileClassEffect(_message.Message):
    __slots__ = ("action", "elements", "elements_omitted", "conditional")
    ACTION_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_OMITTED_FIELD_NUMBER: _ClassVar[int]
    CONDITIONAL_FIELD_NUMBER: _ClassVar[int]
    action: str
    elements: _containers.RepeatedScalarFieldContainer[str]
    elements_omitted: int
    conditional: bool
    def __init__(self, action: _Optional[str] = ..., elements: _Optional[_Iterable[str]] = ..., elements_omitted: _Optional[int] = ..., conditional: _Optional[bool] = ...) -> None: ...

class ProfileMethod(_message.Message):
    __slots__ = ("codes", "withheld", "method_values", "identity_removed", "temporal_modified")
    CODES_FIELD_NUMBER: _ClassVar[int]
    WITHHELD_FIELD_NUMBER: _ClassVar[int]
    METHOD_VALUES_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_REMOVED_FIELD_NUMBER: _ClassVar[int]
    TEMPORAL_MODIFIED_FIELD_NUMBER: _ClassVar[int]
    codes: _containers.RepeatedCompositeFieldContainer[MethodCode]
    withheld: str
    method_values: _containers.RepeatedScalarFieldContainer[str]
    identity_removed: str
    temporal_modified: str
    def __init__(self, codes: _Optional[_Iterable[_Union[MethodCode, _Mapping]]] = ..., withheld: _Optional[str] = ..., method_values: _Optional[_Iterable[str]] = ..., identity_removed: _Optional[str] = ..., temporal_modified: _Optional[str] = ...) -> None: ...

class MethodCode(_message.Message):
    __slots__ = ("value", "scheme", "meaning")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    SCHEME_FIELD_NUMBER: _ClassVar[int]
    MEANING_FIELD_NUMBER: _ClassVar[int]
    value: str
    scheme: str
    meaning: str
    def __init__(self, value: _Optional[str] = ..., scheme: _Optional[str] = ..., meaning: _Optional[str] = ...) -> None: ...
