import React, { Component } from 'react'
import { Form, FormGroup, Input } from 'reactstrap'
import { Field, FieldArray, reduxForm } from 'redux-form'
import { Tooltip } from 'reactstrap';

const renderInput = field => {
  return (
    <Input {...field.input} type={field.type} placeholder={field.placeholder} disabled={field.disabled} />
  )
}

const normalizeInt = (value, _previousValue) => {
  return parseInt(value, 10)
}

const renderListeners = ({ fields }) => (
  <div className="col-xs-12">
    { (fields.length > 0) && <label>Container ports</label>}
    {fields.map((service, i) =>
    <div className="row" key={i}>
      <div className="col-xs-4">
        <div className="form-group">
          <Field name={'listeners['+i+'].port'} component={renderInput} type="number" step="1" min="0" max="65535" normalize={normalizeInt}/>
        </div>
      </div>
      <div className="col-xs-4">
        <div className="form-group service-protocol">
          <label className="form-check-inline">
            <Field className="form-check-input" name={'listeners['+i+'].protocol'} component={renderInput} type="radio" value="TCP"/> TCP
          </label>
          <label className="form-check-inline">
            <Field className="form-check-input" name={'listeners['+i+'].protocol'} component={renderInput} type="radio" value="UDP"/> UDP
          </label>
        </div>
      </div>
      <div className="col-xs-4">
        <button type="button" className="btn btn-secondary btn-sm float-xs-right btn-service-action-right" onClick={() => fields.remove(i)}>
          <i className="fa fa-times" aria-hidden="true" />
        </button>
      </div>
    </div>
    )}
    <div className="row">
      <div className="col-xs-2" style={{ position: 'absolute', zIndex: 100 }}>
        <button type="button" className="btn btn-secondary btn-sm float-xs-left btn-service-action" onClick={() => fields.push({ protocol: 'TCP' })}>Add container port </button>
      </div>
    </div>
  </div>
  )

const renderSpecsSelect = field => {
  return (
    <select {...field.input} name={field.name} className="form-control">
      <option disabled value="">Choose service spec</option>
      {field.serviceSpecs.map(function (spec) {
        return <option key={spec._id} value={spec._id}>{spec.name}</option>
      })}
    </select>
  )
}

class ProjectServiceForm extends Component {
  constructor(props) {
    super(props);

    this.toggleContainerPort = this.toggleContainerPort.bind(this);
    this.state = {
      tooltipContainerPortOpen: false
    };
  }

  toggleContainerPort() {
    this.setState({
      tooltipContainerPortOpen: !this.state.tooltipContainerPortOpen
    })
  }

  render() {
    const { edit, onSave, onCancel, onDelete } = this.props
    return (
        <Form>
          <FormGroup>
            <div className="row">
              <div className="col-xs-12">
                <div className="row">
                  <div className="col-xs-6">
                    <div className="form-group">
                      <label>Name</label>
                      <Field name="name" className="form-control" component={renderInput} disabled={edit} type="text"/>
                    </div>
                  </div>
                  <div className="col-xs-4">
                    <div className="form-group">
                      <label>Spec</label>
                      <Field className="form-control" name="specId" serviceSpecs={this.props.serviceSpecs} component={renderSpecsSelect}/>
                    </div>
                  </div>
                  <div className="col-xs-2">
                    <div className="form-group">
                      <label>Count</label>
                      <Field name="count" component={renderInput} type="number" step="1" min="0" max="100" normalize={normalizeInt}/>
                    </div>
                  </div>
                </div>
                <div className="form-group">
                  <label>Command</label>
                  <Field name="command" className="form-control" component={renderInput} type="text"/>
                </div>
                <div className="form-group">
                  <Field className="form-check-input" name="oneShot" component={renderInput} type="checkbox" value="false"/> One-shot
                </div>
                <div className="row">
                  <div className="col-xs-12">
                    <i className="fa fa-question-circle" id="ToolTipContainerPort" aria-hidden="true" style={{ position: 'absolute', zIndex: 100, bottom: '-20px', left: '175px' }}></i>
                    <Tooltip placement="right" isOpen={this.state.tooltipContainerPortOpen} target="ToolTipContainerPort" toggle={this.toggleContainerPort}>
                      If your application is a webserver then add the port that it listens on.
                    </Tooltip>
                  </div>
                </div>
                <div className="row">
                  <FieldArray name="listeners" component={renderListeners}/>
                </div>
              </div>
              <div className="col-xs-12">
                <button type="button" className="btn btn-secondary btn-sm float-xs-right btn-service-action-right" onClick={() => onCancel()}>
                  <i className="fa fa-times" aria-hidden="true" /> Cancel
                </button>
                { edit &&
                <button type="button" className="btn btn-danger btn-sm float-xs-right btn-service-action-right" onClick={() => onDelete()}>
                  <i className="fa fa-trash" aria-hidden="true" /> Delete
                </button> }
                <button type="button" className="btn btn-success btn-sm float-xs-right btn-service-action-right" onClick={() => onSave()}>
                  <i className="fa fa-check" aria-hidden="true" /> Save
                </button>
              </div>
            </div>
          </FormGroup>
        </Form>
    )
  }
}

export default reduxForm({
  enableReinitialize: true,
  destroyOnUnmount: true,
  form: 'projectService'
})(ProjectServiceForm)
