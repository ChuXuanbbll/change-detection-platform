# 条件与异常

- Ask for an administrative level when multiple regions match.
- Ask for a valid map AOI when the user asks to inspect the current selection and none exists.
- Ask for a complete administrative path when a name is ambiguous or incomplete.
- Report the actual available date precision and never invent a day from a month or year.
- A model or prepared-image listing is a read-only query; never start a detection to answer it.

- `RESOURCE_NOT_FOUND`: ask_for_region
- `SCENE_UNAVAILABLE`: ask_for_date
- `REGION_AMBIGUOUS`: ask_for_administrative_level
